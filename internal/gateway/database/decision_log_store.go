package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"brale/internal/decision"
	"brale/internal/gateway/provider"
	"brale/internal/logger"

	_ "modernc.org/sqlite"
)

// DecisionLogStore 管理实盘 AI 决策日志，方便后续排查/可视化。
type DecisionLogStore struct {
	mu   sync.Mutex
	db   *sql.DB
	path string
}

// DecisionLogRecord 代表一条日志记录，会持久化模型输入/输出摘要。
type DecisionLogRecord struct {
	TraceID    string                      `json:"trace_id"`
	ID         int64                       `json:"id"`
	Timestamp  int64                       `json:"ts"`
	Candidates []string                    `json:"candidates,omitempty"`
	Timeframes []string                    `json:"timeframes,omitempty"`
	Horizon    string                      `json:"horizon"`
	ProviderID string                      `json:"provider_id"`
	Stage      string                      `json:"stage"`
	System     string                      `json:"system_prompt"`
	User       string                      `json:"user_prompt"`
	RawOutput  string                      `json:"raw_output"`
	RawJSON    string                      `json:"raw_json"`
	Meta       string                      `json:"meta_summary"`
	Decisions  []decision.Decision         `json:"decisions"`
	Positions  []decision.PositionSnapshot `json:"positions"`
	Symbols    []string                    `json:"symbols,omitempty"`
	Images     []ImageAttachment           `json:"images,omitempty"`
	Error      string                      `json:"error,omitempty"`
	Note       string                      `json:"note,omitempty"`
}

// ImageAttachment 保存注入模型的图像信息（DataURI + 描述）。
type ImageAttachment struct {
	DataURI     string `json:"data_uri"`
	Description string `json:"description,omitempty"`
}

// LiveDecisionTrace 用于前端展示“决策线”。
type LiveDecisionTrace struct {
	TraceID    string             `json:"trace_id"`
	Timestamp  int64              `json:"ts"`
	Horizon    string             `json:"horizon"`
	Candidates []string           `json:"candidates,omitempty"`
	Timeframes []string           `json:"timeframes,omitempty"`
	Symbols    []string           `json:"symbols,omitempty"`
	Steps      []LiveDecisionStep `json:"steps"`
}

// LiveDecisionStep 描述单次模型请求/响应。
type LiveDecisionStep struct {
	Stage      string              `json:"stage"`
	ProviderID string              `json:"provider_id"`
	Timestamp  int64               `json:"ts"`
	System     string              `json:"system_prompt"`
	User       string              `json:"user_prompt"`
	RawOutput  string              `json:"raw_output"`
	RawJSON    string              `json:"raw_json"`
	Meta       string              `json:"meta_summary"`
	Decisions  []decision.Decision `json:"decisions"`
	Images     []ImageAttachment   `json:"images,omitempty"`
	Error      string              `json:"error,omitempty"`
	Note       string              `json:"note,omitempty"`
}

// LiveOrder 记录实盘执行事件（预留，暂未对外暴露）。
type LiveOrder struct {
	ID        int64   `json:"id"`
	TS        int64   `json:"ts"`
	Symbol    string  `json:"symbol"`
	Action    string  `json:"action"`
	Side      string  `json:"side"`
	Price     float64 `json:"price"`
	Quantity  float64 `json:"quantity"`
	Notional  float64 `json:"notional"`
	Note      string  `json:"note"`
	CreatedAt int64   `json:"created_at"`
}

// LivePosition 记录实盘持仓状态（预留，暂未对外暴露）。
// LiveDecisionQuery 用于筛选实时日志。
type LiveDecisionQuery struct {
	Symbol   string
	Provider string
	Stage    string
	Limit    int
}

// NewDecisionLogStore 初始化 SQLite 存储。
func NewDecisionLogStore(path string) (*DecisionLogStore, error) {
	if path == "" {
		return nil, fmt.Errorf("decision log path 不能为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&cache=shared", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := ensureDecisionLogSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &DecisionLogStore{db: db, path: path}, nil
}

// Close 关闭底层 DB。
func (s *DecisionLogStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func ensureDecisionLogSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS live_decision_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts INTEGER NOT NULL,
			candidates TEXT,
			timeframes TEXT,
			horizon TEXT,
			provider_id TEXT,
			stage TEXT,
			system_prompt TEXT,
			user_prompt TEXT,
			raw_output TEXT,
			raw_json TEXT,
			meta_summary TEXT,
			decisions_json TEXT,
			positions_json TEXT,
			symbols TEXT,
			images_json TEXT,
			error TEXT,
			note TEXT,
			created_at INTEGER NOT NULL,
			trace_id TEXT
		);
		`,
		`CREATE TABLE IF NOT EXISTS live_orders (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts INTEGER NOT NULL,
			symbol TEXT NOT NULL,
			action TEXT NOT NULL,
			side TEXT,
			type TEXT,
			price REAL,
			quantity REAL,
			notional REAL,
			fee REAL,
			timeframe TEXT,
			decided_at INTEGER,
			executed_at INTEGER,
			decision_json TEXT,
			meta_json TEXT,
			take_profit REAL,
			stop_loss REAL,
			expected_rr REAL,
			created_at INTEGER NOT NULL
		);
		`,
		`CREATE TABLE IF NOT EXISTS last_decisions (
			symbol TEXT PRIMARY KEY,
			horizon TEXT,
			decided_at INTEGER NOT NULL,
			decisions_json TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		);
		`,
		`CREATE INDEX IF NOT EXISTS idx_live_logs_ts ON live_decision_logs(ts);`,
		`CREATE INDEX IF NOT EXISTS idx_live_logs_provider ON live_decision_logs(provider_id);`,
		`CREATE INDEX IF NOT EXISTS idx_live_logs_symbol ON live_decision_logs(symbols);`,
		`CREATE INDEX IF NOT EXISTS idx_live_orders_symbol ON live_orders(symbol);`,
		`CREATE INDEX IF NOT EXISTS idx_live_orders_ts ON live_orders(ts);`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return ensureDecisionLogColumns(db)
}

func ensureDecisionLogColumns(db *sql.DB) error {
	cols := []struct {
		table  string
		column string
		typ    string
	}{
		{"live_decision_logs", "trace_id", "TEXT"},
		{"live_decision_logs", "symbols", "TEXT"},
		{"live_decision_logs", "images_json", "TEXT"},
		{"live_orders", "type", "TEXT"},
		{"live_orders", "fee", "REAL"},
		{"live_orders", "timeframe", "TEXT"},
		{"live_orders", "decided_at", "INTEGER"},
		{"live_orders", "executed_at", "INTEGER"},
		{"live_orders", "decision_json", "TEXT"},
		{"live_orders", "meta_json", "TEXT"},
		{"live_orders", "take_profit", "REAL"},
		{"live_orders", "stop_loss", "REAL"},
		{"live_orders", "expected_rr", "REAL"},
		{"last_decisions", "horizon", "TEXT"},
		{"last_decisions", "decisions_json", "TEXT"},
	}
	for _, col := range cols {
		if err := addColumnIfMissing(db, col.table, col.column, col.typ); err != nil {
			return err
		}
	}
	return nil
}

func addColumnIfMissing(db *sql.DB, table, column, typ string) error {
	query := fmt.Sprintf("PRAGMA table_info(%s)", table)
	rows, err := db.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	exists := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if strings.EqualFold(name, column) {
			exists = true
			break
		}
	}
	if exists {
		return nil
	}
	stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, typ)
	_, err = db.Exec(stmt)
	return err
}

// Insert 写入一条日志。
func (s *DecisionLogStore) Insert(ctx context.Context, rec DecisionLogRecord) (int64, error) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return 0, fmt.Errorf("decision log store 未初始化")
	}
	ts := rec.Timestamp
	if ts == 0 {
		ts = time.Now().UnixMilli()
	}
	now := time.Now().UnixMilli()
	if len(rec.Symbols) == 0 && len(rec.Decisions) > 0 {
		rec.Symbols = collectSymbols(rec.Decisions)
	}
	symbolBlob := encodeSymbolBlob(rec.Symbols)
	enc := func(v interface{}) string {
		if v == nil {
			return ""
		}
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
	res, err := db.ExecContext(ctx, `
		INSERT INTO live_decision_logs
			(ts, candidates, timeframes, horizon, provider_id, stage, system_prompt, user_prompt,
			 raw_output, raw_json, meta_summary, decisions_json, positions_json, symbols, images_json, error, note, created_at, trace_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ts,
		enc(rec.Candidates),
		enc(rec.Timeframes),
		rec.Horizon,
		rec.ProviderID,
		rec.Stage,
		rec.System,
		rec.User,
		rec.RawOutput,
		rec.RawJSON,
		rec.Meta,
		enc(rec.Decisions),
		enc(rec.Positions),
		symbolBlob,
		enc(rec.Images),
		rec.Error,
		rec.Note,
		now,
		rec.TraceID,
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// ListDecisions 返回最新的实时决策日志，支持按 provider/stage/symbol 过滤。
func (s *DecisionLogStore) ListDecisions(ctx context.Context, q LiveDecisionQuery) ([]DecisionLogRecord, error) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return nil, fmt.Errorf("decision log store 未初始化")
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var args []interface{}
	var sb strings.Builder
	sb.WriteString(`SELECT id, trace_id, ts, candidates, timeframes, horizon, provider_id, stage,
		system_prompt, user_prompt, raw_output, raw_json, meta_summary, decisions_json,
		positions_json, symbols, images_json, error, note
		FROM live_decision_logs WHERE 1=1`)
	if q.Provider != "" {
		sb.WriteString(" AND provider_id=?")
		args = append(args, q.Provider)
	}
	if q.Stage != "" {
		sb.WriteString(" AND stage=?")
		args = append(args, q.Stage)
	}
	if strings.TrimSpace(q.Symbol) != "" {
		sb.WriteString(" AND symbols LIKE ?")
		args = append(args, symbolLikePattern(q.Symbol))
	}
	sb.WriteString(" ORDER BY ts DESC, id DESC LIMIT ?")
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []DecisionLogRecord
	for rows.Next() {
		var (
			rec        DecisionLogRecord
			candidates sql.NullString
			timeframes sql.NullString
			decisions  sql.NullString
			positions  sql.NullString
			symbols    sql.NullString
			images     sql.NullString
			system     sql.NullString
			user       sql.NullString
			rawOut     sql.NullString
			rawJSON    sql.NullString
			meta       sql.NullString
			errorStr   sql.NullString
			noteStr    sql.NullString
		)
		if err := rows.Scan(&rec.ID, &rec.TraceID, &rec.Timestamp, &candidates, &timeframes, &rec.Horizon,
			&rec.ProviderID, &rec.Stage, &system, &user, &rawOut, &rawJSON, &meta,
			&decisions, &positions, &symbols, &images, &errorStr, &noteStr); err != nil {
			return nil, err
		}
		rec.System = system.String
		rec.User = user.String
		rec.RawOutput = rawOut.String
		rec.RawJSON = rawJSON.String
		rec.Meta = meta.String
		rec.Error = errorStr.String
		rec.Note = noteStr.String
		rec.Candidates = decodeStringArray(candidates.String)
		rec.Timeframes = decodeStringArray(timeframes.String)
		rec.Decisions = decodeDecisionArray(decisions.String)
		rec.Positions = decodePositionArray(positions.String)
		rec.Symbols = decodeSymbolBlob(symbols.String)
		rec.Images = decodeImageArray(images.String)
		list = append(list, rec)
	}
	return list, rows.Err()
}

// DecisionLogObserver 实现 decision.DecisionObserver，将 trace 写入 SQLite。
type DecisionLogObserver struct {
	store *DecisionLogStore
}

// NewDecisionLogObserver 包装 store。
func NewDecisionLogObserver(store *DecisionLogStore) *DecisionLogObserver {
	if store == nil {
		return nil
	}
	return &DecisionLogObserver{store: store}
}

// AfterDecide 记录 provider + final 阶段输出。
func (o *DecisionLogObserver) AfterDecide(ctx context.Context, trace decision.DecisionTrace) {
	if o == nil || o.store == nil {
		return
	}
	base := DecisionLogRecord{
		TraceID:    trace.TraceID,
		Timestamp:  time.Now().UnixMilli(),
		Candidates: cloneStrings(trace.Candidates),
		Timeframes: cloneStrings(trace.Timeframes),
		Horizon:    trace.HorizonName,
		System:     trace.SystemPrompt,
		User:       trace.UserPrompt,
		Positions:  cloneSnapshots(trace.Positions),
	}
	for _, out := range trace.Outputs {
		rec := base
		rec.ProviderID = out.ProviderID
		rec.Stage = "provider"
		rec.RawOutput = out.Raw
		rec.RawJSON = out.Parsed.RawJSON
		rec.Meta = out.Parsed.MetaSummary
		rec.Decisions = append([]decision.Decision(nil), out.Parsed.Decisions...)
		rec.Symbols = collectSymbols(rec.Decisions)
		rec.Images = attachmentsFromProviderImages(out.Images)
		rec.Note = "provider"
		if out.Err != nil {
			rec.Error = out.Err.Error()
		}
		if _, err := o.store.Insert(ctx, rec); err != nil {
			logger.Warnf("写入决策日志失败(provider): %v", err)
		}
	}
	finalRec := base
	finalRec.Stage = "final"
	finalRec.Note = "final"
	finalRec.ProviderID = trace.Best.ProviderID
	if finalRec.ProviderID == "" {
		finalRec.ProviderID = "aggregate"
	}
	finalRec.RawOutput = trace.Best.Raw
	finalRec.RawJSON = trace.Best.Parsed.RawJSON
	finalRec.Meta = trace.Best.Parsed.MetaSummary
	finalRec.Decisions = append([]decision.Decision(nil), trace.Best.Parsed.Decisions...)
	finalRec.Symbols = collectSymbols(finalRec.Decisions)
	finalRec.Images = attachmentsFromProviderImages(trace.Best.Images)
	if trace.Best.Err != nil {
		finalRec.Error = trace.Best.Err.Error()
	}
	if _, err := o.store.Insert(ctx, finalRec); err != nil {
		logger.Warnf("写入决策日志失败(final): %v", err)
	}
	if len(trace.AgentInsights) > 0 {
		for _, ins := range trace.AgentInsights {
			stage := strings.TrimSpace(ins.Stage)
			if stage == "" {
				continue
			}
			rec := base
			rec.Stage = "agent:" + stage
			rec.ProviderID = ins.ProviderID
			if sys := strings.TrimSpace(ins.System); sys != "" {
				rec.System = sys
			}
			if usr := strings.TrimSpace(ins.User); usr != "" {
				rec.User = usr
			}
			rec.RawOutput = ins.Output
			rec.RawJSON = ""
			rec.Meta = ""
			rec.Decisions = nil
			rec.Symbols = nil
			rec.Note = "agent"
			if ins.Warned {
				rec.Note += "|warned"
			}
			rec.Error = ins.Error
			if _, err := o.store.Insert(ctx, rec); err != nil {
				logger.Warnf("写入决策日志失败(agent:%s): %v", stage, err)
			}
		}
	}
}

// BuildLiveDecisionTraces 将平铺日志转为按 trace_id 分组的决策线。
func BuildLiveDecisionTraces(records []DecisionLogRecord) []LiveDecisionTrace {
	if len(records) == 0 {
		return nil
	}
	type stepWrap struct {
		step  LiveDecisionStep
		order int64
	}
	type builder struct {
		trace LiveDecisionTrace
		steps []stepWrap
	}
	groups := make(map[string]*builder)
	var orderKeys []string
	for idx, rec := range records {
		key := strings.TrimSpace(rec.TraceID)
		if key == "" {
			key = fmt.Sprintf("legacy-%d-%s-%s-%d", rec.Timestamp, rec.Horizon, strings.Join(rec.Symbols, "|"), idx)
		}
		b := groups[key]
		if b == nil {
			trace := LiveDecisionTrace{
				TraceID:    key,
				Timestamp:  rec.Timestamp,
				Horizon:    rec.Horizon,
				Candidates: cloneStrings(rec.Candidates),
				Timeframes: cloneStrings(rec.Timeframes),
				Symbols:    cloneStrings(rec.Symbols),
			}
			b = &builder{trace: trace}
			groups[key] = b
			orderKeys = append(orderKeys, key)
		} else if rec.Timestamp > b.trace.Timestamp {
			b.trace.Timestamp = rec.Timestamp
		}
		if len(rec.Symbols) > 0 {
			b.trace.Symbols = mergeSymbolLists(b.trace.Symbols, rec.Symbols)
		}
		step := LiveDecisionStep{
			Stage:      rec.Stage,
			ProviderID: rec.ProviderID,
			Timestamp:  rec.Timestamp,
			System:     rec.System,
			User:       rec.User,
			RawOutput:  rec.RawOutput,
			RawJSON:    rec.RawJSON,
			Meta:       rec.Meta,
			Images:     cloneImages(rec.Images),
			Error:      rec.Error,
			Note:       rec.Note,
		}
		if len(rec.Decisions) > 0 {
			step.Decisions = append([]decision.Decision(nil), rec.Decisions...)
		}
		order := (rec.Timestamp << 20) + rec.ID
		b.steps = append(b.steps, stepWrap{step: step, order: order})
	}
	for _, b := range groups {
		sort.Slice(b.steps, func(i, j int) bool {
			return b.steps[i].order < b.steps[j].order
		})
		steps := make([]LiveDecisionStep, len(b.steps))
		for i, sp := range b.steps {
			steps[i] = sp.step
		}
		b.trace.Steps = steps
	}
	sort.Slice(orderKeys, func(i, j int) bool {
		return groups[orderKeys[i]].trace.Timestamp > groups[orderKeys[j]].trace.Timestamp
	})
	out := make([]LiveDecisionTrace, len(orderKeys))
	for i, key := range orderKeys {
		out[i] = groups[key].trace
	}
	return out
}

func cloneStrings(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	dst := make([]string, len(src))
	copy(dst, src)
	return dst
}

func cloneSnapshots(src []decision.PositionSnapshot) []decision.PositionSnapshot {
	if len(src) == 0 {
		return nil
	}
	dst := make([]decision.PositionSnapshot, len(src))
	copy(dst, src)
	return dst
}

func cloneImages(src []ImageAttachment) []ImageAttachment {
	if len(src) == 0 {
		return nil
	}
	dst := make([]ImageAttachment, len(src))
	copy(dst, src)
	return dst
}

func collectSymbols(decisions []decision.Decision) []string {
	if len(decisions) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, d := range decisions {
		sym := strings.ToUpper(strings.TrimSpace(d.Symbol))
		if sym == "" {
			continue
		}
		if _, ok := seen[sym]; ok {
			continue
		}
		seen[sym] = struct{}{}
		out = append(out, sym)
	}
	return out
}

func mergeSymbolLists(dst, src []string) []string {
	if len(src) == 0 {
		return dst
	}
	seen := make(map[string]struct{}, len(dst)+len(src))
	var out []string
	appendSymbol := func(sym string) {
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if sym == "" {
			return
		}
		if _, ok := seen[sym]; ok {
			return
		}
		seen[sym] = struct{}{}
		out = append(out, sym)
	}
	for _, sym := range dst {
		appendSymbol(sym)
	}
	for _, sym := range src {
		appendSymbol(sym)
	}
	return out
}

func encodeSymbolBlob(symbols []string) string {
	if len(symbols) == 0 {
		return ""
	}
	seen := make(map[string]struct{})
	var cleaned []string
	for _, sym := range symbols {
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if sym == "" {
			continue
		}
		if _, ok := seen[sym]; ok {
			continue
		}
		seen[sym] = struct{}{}
		cleaned = append(cleaned, sym)
	}
	if len(cleaned) == 0 {
		return ""
	}
	return "|" + strings.Join(cleaned, "|") + "|"
}

func decodeSymbolBlob(blob string) []string {
	blob = strings.Trim(blob, "|")
	if blob == "" {
		return nil
	}
	parts := strings.Split(blob, "|")
	seen := make(map[string]struct{})
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func decodeStringArray(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var arr []string
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		logger.Warnf("解析字符串数组失败: %v", err)
		return nil
	}
	return arr
}

func decodeDecisionArray(raw string) []decision.Decision {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var arr []decision.Decision
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		logger.Warnf("解析决策数组失败: %v", err)
		return nil
	}
	return arr
}

func decodePositionArray(raw string) []decision.PositionSnapshot {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var arr []decision.PositionSnapshot
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		logger.Warnf("解析持仓数组失败: %v", err)
		return nil
	}
	return arr
}

func decodeImageArray(raw string) []ImageAttachment {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var arr []ImageAttachment
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		logger.Warnf("解析图像附件失败: %v", err)
		return nil
	}
	cleaned := make([]ImageAttachment, 0, len(arr))
	for _, img := range arr {
		img.DataURI = strings.TrimSpace(img.DataURI)
		img.Description = strings.TrimSpace(img.Description)
		if img.DataURI == "" {
			continue
		}
		cleaned = append(cleaned, img)
	}
	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}

func symbolLikePattern(sym string) string {
	sym = strings.ToUpper(strings.TrimSpace(sym))
	if sym == "" {
		return "%"
	}
	return "%|" + sym + "|%"
}

func attachmentsFromProviderImages(images []provider.ImagePayload) []ImageAttachment {
	if len(images) == 0 {
		return nil
	}
	out := make([]ImageAttachment, 0, len(images))
	for _, img := range images {
		data := strings.TrimSpace(img.DataURI)
		if data == "" {
			continue
		}
		desc := strings.TrimSpace(img.Description)
		out = append(out, ImageAttachment{DataURI: data, Description: desc})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
