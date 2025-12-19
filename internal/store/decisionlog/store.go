package decisionlog

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

type DecisionLogStore struct {
	mu     sync.Mutex
	db     *sql.DB
	path   string
	ownsDB bool

	agentCacheMu     sync.RWMutex
	agentOutputCache map[agentOutputCacheKey]agentOutputCacheEntry
}

type agentOutputCacheKey struct {
	Symbol     string
	Stage      string
	ProviderID string
}

type agentOutputCacheEntry struct {
	Output string
	TS     int64
}

type DecisionLogRecord struct {
	TraceID         string                      `json:"trace_id"`
	ID              int64                       `json:"id"`
	Timestamp       int64                       `json:"ts"`
	Candidates      []string                    `json:"candidates,omitempty"`
	Timeframes      []string                    `json:"timeframes,omitempty"`
	Horizon         string                      `json:"horizon"`
	ProviderID      string                      `json:"provider_id"`
	Stage           string                      `json:"stage"`
	System          string                      `json:"system_prompt"`
	User            string                      `json:"user_prompt"`
	RawOutput       string                      `json:"raw_output"`
	RawJSON         string                      `json:"raw_json"`
	Meta            string                      `json:"meta_summary"`
	Decisions       []decision.Decision         `json:"decisions"`
	Positions       []decision.PositionSnapshot `json:"positions"`
	Symbols         []string                    `json:"symbols,omitempty"`
	Images          []ImageAttachment           `json:"images,omitempty"`
	VisionSupported bool                        `json:"vision_supported"`
	ImageCount      int                         `json:"image_count"`
	Error           string                      `json:"error,omitempty"`
	Note            string                      `json:"note,omitempty"`
}

type ImageAttachment struct {
	DataURI     string `json:"data_uri"`
	Description string `json:"description,omitempty"`
}

type LiveDecisionTrace struct {
	TraceID    string             `json:"trace_id"`
	Timestamp  int64              `json:"ts"`
	Horizon    string             `json:"horizon"`
	Candidates []string           `json:"candidates,omitempty"`
	Timeframes []string           `json:"timeframes,omitempty"`
	Symbols    []string           `json:"symbols,omitempty"`
	Steps      []LiveDecisionStep `json:"steps"`
}

type LiveDecisionStep struct {
	Stage           string              `json:"stage"`
	ProviderID      string              `json:"provider_id"`
	Timestamp       int64               `json:"ts"`
	System          string              `json:"system_prompt"`
	User            string              `json:"user_prompt"`
	RawOutput       string              `json:"raw_output"`
	RawJSON         string              `json:"raw_json"`
	Meta            string              `json:"meta_summary"`
	Decisions       []decision.Decision `json:"decisions"`
	Images          []ImageAttachment   `json:"images,omitempty"`
	VisionSupported bool                `json:"vision_supported"`
	ImageCount      int                 `json:"image_count"`
	Error           string              `json:"error,omitempty"`
	Note            string              `json:"note,omitempty"`
}

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

type LiveDecisionQuery struct {
	Symbol   string
	Symbols  []string
	Provider string
	Stage    string
	Limit    int
	Offset   int
}

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
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	if err := ensureDecisionLogSchema(db); err != nil {
		if cerr := db.Close(); cerr != nil {
			logger.Warnf("decision log db close failed: %v", cerr)
		}
		return nil, err
	}
	return &DecisionLogStore{
		db:               db,
		path:             path,
		ownsDB:           true,
		agentOutputCache: make(map[agentOutputCacheKey]agentOutputCacheEntry),
	}, nil
}

func (s *DecisionLogStore) UseExternalDB(db *sql.DB) error {
	if s == nil {
		return fmt.Errorf("decision log store 未初始化")
	}
	if db == nil {
		return fmt.Errorf("external db 不能为空")
	}
	if err := ensureDecisionLogSchema(db); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ownsDB && s.db != nil && s.db != db {
		_ = s.db.Close()
	}
	s.db = db
	s.ownsDB = false
	if s.agentOutputCache == nil {
		s.agentOutputCache = make(map[agentOutputCacheKey]agentOutputCacheEntry)
	}
	return nil
}

func (s *DecisionLogStore) Close() error {
	s.agentCacheMu.Lock()
	s.agentOutputCache = nil
	s.agentCacheMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	if !s.ownsDB {
		s.db = nil
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
			vision_supported INTEGER,
			image_count INTEGER,
			error TEXT,
			note TEXT,
			created_at INTEGER NOT NULL,
			trace_id TEXT
		);
		`,
		`CREATE INDEX IF NOT EXISTS idx_live_decision_logs_stage_ts_id ON live_decision_logs(stage, ts DESC, id DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_live_decision_logs_provider_stage_ts_id ON live_decision_logs(provider_id, stage, ts DESC, id DESC);`,
		`CREATE TABLE IF NOT EXISTS live_orders (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			freqtrade_id INTEGER NOT NULL UNIQUE,
			symbol TEXT NOT NULL,
			side TEXT NOT NULL,
			amount REAL NOT NULL DEFAULT 0,
			initial_amount REAL NOT NULL DEFAULT 0,
			stake_amount REAL NOT NULL DEFAULT 0,
			leverage REAL NOT NULL DEFAULT 1,
			position_value REAL NOT NULL DEFAULT 0,
			price REAL NOT NULL DEFAULT 0,
			closed_amount REAL NOT NULL DEFAULT 0,
			pnl_ratio REAL DEFAULT 0,
			pnl_usd REAL DEFAULT 0,
			current_price REAL DEFAULT 0,
			current_profit_ratio REAL DEFAULT 0,
			current_profit_abs REAL DEFAULT 0,
			unrealized_pnl_ratio REAL DEFAULT 0,
			unrealized_pnl_usd REAL DEFAULT 0,
			realized_pnl_ratio REAL DEFAULT 0,
			realized_pnl_usd REAL DEFAULT 0,
			is_simulated INTEGER NOT NULL DEFAULT 0,
			status INTEGER NOT NULL,
			start_timestamp INTEGER NOT NULL,
			end_timestamp INTEGER,
			last_status_sync INTEGER,
			raw_data TEXT,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		`,
		`CREATE INDEX IF NOT EXISTS idx_live_orders_status_updated ON live_orders(status, updated_at);`,
		`CREATE INDEX IF NOT EXISTS idx_live_orders_symbol_status ON live_orders(symbol, status);`,

		`CREATE TABLE IF NOT EXISTS strategy_instances (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trade_id INTEGER NOT NULL,
			plan_id TEXT NOT NULL,
			plan_component TEXT NOT NULL,
			plan_version INTEGER NOT NULL DEFAULT 1,
			params_json TEXT NOT NULL,
			state_json TEXT,
			status INTEGER NOT NULL DEFAULT 0,
			decision_trace_id TEXT,
			last_eval_at INTEGER,
			next_eval_after INTEGER,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		`,
		`CREATE INDEX IF NOT EXISTS idx_strategy_instances_trade_id ON strategy_instances(trade_id);`,
		`CREATE TABLE IF NOT EXISTS strategy_change_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trade_id INTEGER NOT NULL,
			instance_id INTEGER,
			plan_id TEXT NOT NULL,
			plan_component TEXT,
			changed_field TEXT NOT NULL,
			old_value TEXT,
			new_value TEXT,
			trigger_source TEXT,
			reason TEXT,
			decision_trace_id TEXT,
			created_at INTEGER NOT NULL
		);
		`,
		`CREATE TABLE IF NOT EXISTS trade_operation_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			freqtrade_id INTEGER NOT NULL,
			symbol TEXT NOT NULL,
			operation INTEGER NOT NULL,
			details TEXT NOT NULL,
			timestamp INTEGER NOT NULL
		);
		`,
		`CREATE INDEX IF NOT EXISTS idx_live_logs_ts ON live_decision_logs(ts);`,
		`CREATE INDEX IF NOT EXISTS idx_live_logs_provider ON live_decision_logs(provider_id);`,
		`CREATE INDEX IF NOT EXISTS idx_live_logs_symbol ON live_decision_logs(symbols);`,
		`CREATE INDEX IF NOT EXISTS idx_live_logs_trace_id ON live_decision_logs(trace_id);`,
		`CREATE INDEX IF NOT EXISTS idx_live_orders_symbol ON live_orders(symbol);`,
		`CREATE INDEX IF NOT EXISTS idx_live_orders_status ON live_orders(status);`,

		`CREATE INDEX IF NOT EXISTS idx_strategy_instances_trade ON strategy_instances(trade_id);`,
		`CREATE INDEX IF NOT EXISTS idx_strategy_instances_status ON strategy_instances(status);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_strategy_instance ON strategy_instances(trade_id, plan_id, plan_component);`,
		`CREATE INDEX IF NOT EXISTS idx_strategy_change_trade ON strategy_change_log(trade_id);`,
		`CREATE INDEX IF NOT EXISTS idx_strategy_change_plan ON strategy_change_log(plan_id);`,
		`CREATE INDEX IF NOT EXISTS idx_trade_operation_freqtrade ON trade_operation_log(freqtrade_id);`,
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
		{"live_decision_logs", "vision_supported", "INTEGER"},
		{"live_decision_logs", "image_count", "INTEGER"},
		{"live_orders", "position_value", "REAL NOT NULL DEFAULT 0"},
		{"live_orders", "pnl_ratio", "REAL DEFAULT 0"},
		{"live_orders", "pnl_usd", "REAL DEFAULT 0"},
		{"live_orders", "current_price", "REAL DEFAULT 0"},
		{"live_orders", "current_profit_ratio", "REAL DEFAULT 0"},
		{"live_orders", "current_profit_abs", "REAL DEFAULT 0"},
		{"live_orders", "unrealized_pnl_ratio", "REAL DEFAULT 0"},
		{"live_orders", "unrealized_pnl_usd", "REAL DEFAULT 0"},
		{"live_orders", "realized_pnl_ratio", "REAL DEFAULT 0"},
		{"live_orders", "realized_pnl_usd", "REAL DEFAULT 0"},
		{"live_orders", "last_status_sync", "INTEGER"},
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
	defer func() { _ = rows.Close() }()
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
			 raw_output, raw_json, meta_summary, decisions_json, positions_json, symbols, images_json,
			 vision_supported, image_count, error, note, created_at, trace_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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
		boolToInt(rec.VisionSupported),
		rec.ImageCount,
		rec.Error,
		rec.Note,
		now,
		rec.TraceID,
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	s.maybeCacheAgentOutput(rec, ts)
	return id, nil
}

func (s *DecisionLogStore) maybeCacheAgentOutput(rec DecisionLogRecord, ts int64) {
	if s == nil {
		return
	}
	stage := strings.TrimSpace(rec.Stage)
	if !strings.HasPrefix(stage, "agent:") {
		return
	}
	providerID := strings.TrimSpace(rec.ProviderID)
	if providerID == "" {
		return
	}
	output := strings.TrimSpace(rec.RawOutput)
	if output == "" {
		return
	}
	if len(rec.Symbols) == 0 {
		return
	}
	s.agentCacheMu.Lock()
	if s.agentOutputCache == nil {
		s.agentOutputCache = make(map[agentOutputCacheKey]agentOutputCacheEntry)
	}
	for _, sym := range rec.Symbols {
		symbol := strings.ToUpper(strings.TrimSpace(sym))
		if symbol == "" {
			continue
		}
		key := agentOutputCacheKey{Symbol: symbol, Stage: stage, ProviderID: providerID}
		prev, ok := s.agentOutputCache[key]
		if !ok || ts >= prev.TS {
			s.agentOutputCache[key] = agentOutputCacheEntry{Output: output, TS: ts}
		}
	}
	s.agentCacheMu.Unlock()
}

func buildLiveDecisionFilter(q LiveDecisionQuery) (string, []interface{}) {
	var args []interface{}
	var sb strings.Builder
	sb.WriteString(" WHERE 1=1")
	if q.Provider != "" {
		sb.WriteString(" AND provider_id=?")
		args = append(args, q.Provider)
	}
	stage := strings.TrimSpace(q.Stage)
	if stage == "" {
		stage = "core"
	}
	switch stage {
	case "core":
		sb.WriteString(" AND (stage='final' OR stage='provider' OR stage LIKE 'agent:%')")
	case "all":

	default:
		sb.WriteString(" AND stage=?")
		args = append(args, stage)
	}
	symbols := normalizeSymbols(q.Symbols)
	if len(symbols) == 0 && strings.TrimSpace(q.Symbol) != "" {
		symbols = []string{strings.ToUpper(strings.TrimSpace(q.Symbol))}
	}
	if len(symbols) > 0 {
		sb.WriteString(" AND (")
		for i, sym := range symbols {
			if i > 0 {
				sb.WriteString(" OR ")
			}
			sb.WriteString("symbols LIKE ?")
			args = append(args, symbolLikePattern(sym))
		}
		sb.WriteString(")")
	}
	return sb.String(), args
}

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanDecisionLogRecord(scanner rowScanner) (DecisionLogRecord, error) {
	var (
		rec        DecisionLogRecord
		candidates sql.NullString
		timeframes sql.NullString
		decisions  sql.NullString
		positions  sql.NullString
		symbols    sql.NullString
		images     sql.NullString
		vision     sql.NullInt64
		imageCount sql.NullInt64
		system     sql.NullString
		user       sql.NullString
		rawOut     sql.NullString
		rawJSON    sql.NullString
		meta       sql.NullString
		errorStr   sql.NullString
		noteStr    sql.NullString
	)
	if err := scanner.Scan(&rec.ID, &rec.TraceID, &rec.Timestamp, &candidates, &timeframes, &rec.Horizon,
		&rec.ProviderID, &rec.Stage, &system, &user, &rawOut, &rawJSON, &meta,
		&decisions, &positions, &symbols, &images, &vision, &imageCount, &errorStr, &noteStr); err != nil {
		return rec, err
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
	rec.VisionSupported = nullIntToBool(vision)
	if imageCount.Valid {
		rec.ImageCount = int(imageCount.Int64)
	} else if len(rec.Images) > 0 {
		rec.ImageCount = len(rec.Images)
	}
	return rec, nil
}

func (s *DecisionLogStore) GetDecision(ctx context.Context, id int64) (DecisionLogRecord, error) {
	var rec DecisionLogRecord
	if id <= 0 {
		return rec, fmt.Errorf("invalid decision id")
	}
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return rec, fmt.Errorf("decision log store 未初始化")
	}
	row := db.QueryRowContext(ctx, `SELECT id, trace_id, ts, candidates, timeframes, horizon, provider_id, stage,
		system_prompt, user_prompt, raw_output, raw_json, meta_summary, decisions_json,
		positions_json, symbols, images_json, vision_supported, image_count, error, note
		FROM live_decision_logs WHERE id = ?`, id)
	return scanDecisionLogRecord(row)
}

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
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	filterSQL, args := buildLiveDecisionFilter(q)
	var sb strings.Builder
	sb.WriteString(`SELECT id, trace_id, ts, candidates, timeframes, horizon, provider_id, stage,
		system_prompt, user_prompt, raw_output, raw_json, meta_summary, decisions_json,
		positions_json, symbols, images_json, vision_supported, image_count, error, note
		FROM live_decision_logs`)
	sb.WriteString(filterSQL)
	sb.WriteString(" ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?")
	args = append(args, limit, offset)
	rows, err := db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var list []DecisionLogRecord
	for rows.Next() {
		rec, err := scanDecisionLogRecord(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, rec)
	}
	return list, rows.Err()
}

func (s *DecisionLogStore) CountDecisions(ctx context.Context, q LiveDecisionQuery) (int, error) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return 0, fmt.Errorf("decision log store 未初始化")
	}
	filterSQL, args := buildLiveDecisionFilter(q)
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM live_decision_logs`+filterSQL, args...).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

func (s *DecisionLogStore) ListDecisionsByTraceID(ctx context.Context, traceID string, limit int) ([]DecisionLogRecord, error) {
	traceID = strings.TrimSpace(traceID)
	if traceID == "" {
		return nil, fmt.Errorf("trace_id 不能为空")
	}
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return nil, fmt.Errorf("decision log store 未初始化")
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := db.QueryContext(ctx, `SELECT id, trace_id, ts, candidates, timeframes, horizon, provider_id, stage,
		system_prompt, user_prompt, raw_output, raw_json, meta_summary, decisions_json,
		positions_json, symbols, images_json, vision_supported, image_count, error, note
		FROM live_decision_logs WHERE trace_id = ?
		ORDER BY ts ASC, id ASC
		LIMIT ?`, traceID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var list []DecisionLogRecord
	for rows.Next() {
		rec, err := scanDecisionLogRecord(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, rec)
	}
	return list, rows.Err()
}

func (s *DecisionLogStore) LatestAgentOutput(ctx context.Context, symbol, stage, providerID string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("decision log store 未初始化")
	}
	key, err := normalizeAgentOutputKey(symbol, stage, providerID)
	if err != nil {
		return "", err
	}
	if out, ok := s.getAgentOutputCache(key); ok {
		return out, nil
	}
	if ctx != nil && ctx.Err() != nil {
		return "", nil
	}
	out, err := s.queryLatestAgentOutput(ctx, key)
	if err != nil || out == "" {
		return out, err
	}
	s.putAgentOutputCache(key, out)
	return out, nil
}

func normalizeAgentOutputKey(symbol, stage, providerID string) (agentOutputCacheKey, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	stage = strings.TrimSpace(stage)
	providerID = strings.TrimSpace(providerID)
	if symbol == "" {
		return agentOutputCacheKey{}, fmt.Errorf("symbol 不能为空")
	}
	if stage == "" {
		return agentOutputCacheKey{}, fmt.Errorf("stage 不能为空")
	}
	if providerID == "" {
		return agentOutputCacheKey{}, fmt.Errorf("provider_id 不能为空")
	}
	if !strings.HasPrefix(stage, "agent:") {
		stage = "agent:" + stage
	}
	return agentOutputCacheKey{Symbol: symbol, Stage: stage, ProviderID: providerID}, nil
}

func (s *DecisionLogStore) getAgentOutputCache(key agentOutputCacheKey) (string, bool) {
	s.agentCacheMu.RLock()
	defer s.agentCacheMu.RUnlock()
	if s.agentOutputCache == nil {
		return "", false
	}
	hit, ok := s.agentOutputCache[key]
	if !ok {
		return "", false
	}
	out := strings.TrimSpace(hit.Output)
	if out == "" {
		return "", false
	}
	return out, true
}

func (s *DecisionLogStore) putAgentOutputCache(key agentOutputCacheKey, out string) {
	out = strings.TrimSpace(out)
	if out == "" {
		return
	}
	s.agentCacheMu.Lock()
	defer s.agentCacheMu.Unlock()
	if s.agentOutputCache == nil {
		s.agentOutputCache = make(map[agentOutputCacheKey]agentOutputCacheEntry)
	}
	prev, ok := s.agentOutputCache[key]
	if !ok || prev.TS <= 0 {
		s.agentOutputCache[key] = agentOutputCacheEntry{Output: out, TS: time.Now().UnixMilli()}
	}
}

func (s *DecisionLogStore) queryLatestAgentOutput(ctx context.Context, key agentOutputCacheKey) (string, error) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return "", fmt.Errorf("decision log store 未初始化")
	}
	row := db.QueryRowContext(ctx, `SELECT raw_output FROM live_decision_logs
		WHERE stage = ? AND provider_id = ? AND symbols LIKE ? AND raw_output IS NOT NULL AND raw_output != ''
		ORDER BY ts DESC, id DESC
		LIMIT 1`, key.Stage, key.ProviderID, symbolLikePattern(key.Symbol))
	var raw sql.NullString
	if err := row.Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(raw.String), nil
}

func (s *DecisionLogStore) ListDecisionsByTraceIDs(ctx context.Context, traceIDs []string) (map[string][]DecisionLogRecord, error) {
	clean := normalizeTraceIDs(traceIDs)
	if len(clean) == 0 {
		return map[string][]DecisionLogRecord{}, nil
	}
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return nil, fmt.Errorf("decision log store 未初始化")
	}
	var placeholders []string
	args := make([]interface{}, 0, len(clean))
	for _, id := range clean {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	query := fmt.Sprintf(`SELECT id, trace_id, ts, candidates, timeframes, horizon, provider_id, stage,
		system_prompt, user_prompt, raw_output, raw_json, meta_summary, decisions_json,
		positions_json, symbols, images_json, vision_supported, image_count, error, note
		FROM live_decision_logs
		WHERE trace_id IN (%s)
		ORDER BY trace_id ASC, ts ASC, id ASC`, strings.Join(placeholders, ","))
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make(map[string][]DecisionLogRecord, len(clean))
	for rows.Next() {
		rec, err := scanDecisionLogRecord(rows)
		if err != nil {
			return nil, err
		}
		key := strings.TrimSpace(rec.TraceID)
		if key == "" {
			key = deriveLegacyTraceKey(rec, int(rec.ID))
		}
		result[key] = append(result[key], rec)
	}
	return result, rows.Err()
}

type DecisionLogObserver struct {
	store *DecisionLogStore
}

func NewDecisionLogObserver(store *DecisionLogStore) *DecisionLogObserver {
	if store == nil {
		return nil
	}
	return &DecisionLogObserver{store: store}
}

func (o *DecisionLogObserver) AfterDecide(ctx context.Context, trace decision.DecisionTrace) {
	if o == nil || o.store == nil {
		return
	}
	candidateSymbols := normalizeSymbols(trace.Candidates)
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
		o.logProviderDecision(ctx, base, out, candidateSymbols)
	}
	o.logFinalDecision(ctx, base, trace, candidateSymbols)
	o.logAgentInsights(ctx, base, trace.AgentInsights, candidateSymbols)
}

func (o *DecisionLogObserver) logProviderDecision(ctx context.Context, base DecisionLogRecord, out decision.ModelOutput, candidateSymbols []string) {
	rec := base
	rec.ProviderID = out.ProviderID
	rec.Stage = "provider"
	if sys := strings.TrimSpace(out.SystemPrompt); sys != "" {
		rec.System = sys
	}
	if usr := strings.TrimSpace(out.UserPrompt); usr != "" {
		rec.User = usr
	}
	rec.RawOutput = out.Raw
	rec.RawJSON = out.Parsed.RawJSON
	rec.Meta = out.Parsed.MetaSummary
	rec.Decisions = append([]decision.Decision(nil), out.Parsed.Decisions...)
	rec.Symbols = mergeSymbolLists(collectSymbols(rec.Decisions), candidateSymbols)
	rec.Images = attachmentsFromProviderImages(out.Images)
	rec.VisionSupported = out.VisionEnabled
	rec.ImageCount = len(out.Images)
	rec.Note = "provider"
	if out.Err != nil {
		rec.Error = out.Err.Error()
	}
	if _, err := o.store.Insert(ctx, rec); err != nil {
		logger.Warnf("写入决策日志失败(provider): %v", err)
	}
}

func (o *DecisionLogObserver) logFinalDecision(ctx context.Context, base DecisionLogRecord, trace decision.DecisionTrace, candidateSymbols []string) {
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
	if sys := strings.TrimSpace(trace.Best.SystemPrompt); sys != "" {
		finalRec.System = sys
	}
	if usr := strings.TrimSpace(trace.Best.UserPrompt); usr != "" {
		finalRec.User = usr
	}
	finalRec.Decisions = append([]decision.Decision(nil), trace.Best.Parsed.Decisions...)
	finalRec.Symbols = mergeSymbolLists(collectSymbols(finalRec.Decisions), candidateSymbols)
	finalRec.Images = attachmentsFromProviderImages(trace.Best.Images)
	finalRec.VisionSupported = trace.Best.VisionEnabled
	finalRec.ImageCount = len(trace.Best.Images)
	if trace.Best.Err != nil {
		finalRec.Error = trace.Best.Err.Error()
	}
	if _, err := o.store.Insert(ctx, finalRec); err != nil {
		logger.Warnf("写入决策日志失败(final): %v", err)
	}
}

func (o *DecisionLogObserver) logAgentInsights(ctx context.Context, base DecisionLogRecord, insights []decision.AgentInsight, candidateSymbols []string) {
	for _, ins := range insights {
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
		rec.Symbols = mergeSymbolLists(nil, candidateSymbols)
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
		key := canonicalTraceKey(rec, idx)
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
			Stage:           rec.Stage,
			ProviderID:      rec.ProviderID,
			Timestamp:       rec.Timestamp,
			System:          rec.System,
			User:            rec.User,
			RawOutput:       rec.RawOutput,
			RawJSON:         rec.RawJSON,
			Meta:            rec.Meta,
			Images:          cloneImages(rec.Images),
			VisionSupported: rec.VisionSupported,
			ImageCount:      rec.ImageCount,
			Error:           rec.Error,
			Note:            rec.Note,
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

func canonicalTraceKey(rec DecisionLogRecord, idx int) string {
	key := strings.TrimSpace(rec.TraceID)
	if key != "" {
		return key
	}
	return deriveLegacyTraceKey(rec, idx)
}

func deriveLegacyTraceKey(rec DecisionLogRecord, idx int) string {
	suffix := idx
	if suffix <= 0 {
		suffix = int(rec.ID)
	}
	symbolBlob := strings.Join(rec.Symbols, "|")
	if symbolBlob == "" {
		symbolBlob = "unknown"
	}
	return fmt.Sprintf("legacy-%d-%s-%s-%d", rec.Timestamp, rec.Horizon, symbolBlob, suffix)
}

type DecisionRoundSummary struct {
	TraceID       string              `json:"trace_id"`
	RoundKey      string              `json:"round_key"`
	StartedAt     int64               `json:"started_at"`
	CompletedAt   int64               `json:"completed_at"`
	Horizon       string              `json:"horizon"`
	Symbols       []string            `json:"symbols"`
	Candidates    []string            `json:"candidates"`
	Timeframes    []string            `json:"timeframes"`
	TotalSteps    int                 `json:"total_steps"`
	ProviderSteps int                 `json:"provider_steps"`
	AgentSteps    int                 `json:"agent_steps"`
	ErrorSteps    int                 `json:"error_steps"`
	ProviderIDs   []string            `json:"provider_ids"`
	FinalDecision DecisionLogRecord   `json:"final_decision"`
	Steps         []DecisionLogRecord `json:"steps,omitempty"`
}

func BuildDecisionRoundSummaries(finals []DecisionLogRecord, traceLogs map[string][]DecisionLogRecord) []DecisionRoundSummary {
	if len(finals) == 0 {
		return nil
	}
	out := make([]DecisionRoundSummary, 0, len(finals))
	for _, final := range finals {
		trimmedTrace := strings.TrimSpace(final.TraceID)
		steps := traceLogs[trimmedTrace]
		if len(steps) == 0 {
			steps = []DecisionLogRecord{final}
		}
		summary := DecisionRoundSummary{
			TraceID:       trimmedTrace,
			RoundKey:      canonicalTraceKey(final, int(final.ID)),
			StartedAt:     final.Timestamp,
			CompletedAt:   final.Timestamp,
			Horizon:       final.Horizon,
			Symbols:       cloneStrings(final.Symbols),
			Candidates:    cloneStrings(final.Candidates),
			Timeframes:    cloneStrings(final.Timeframes),
			FinalDecision: final,
		}
		providerSet := make(map[string]struct{})
		for _, step := range steps {
			if step.Timestamp < summary.StartedAt {
				summary.StartedAt = step.Timestamp
			}
			if step.Timestamp > summary.CompletedAt {
				summary.CompletedAt = step.Timestamp
			}
			if len(step.Symbols) > 0 {
				summary.Symbols = mergeSymbolLists(summary.Symbols, step.Symbols)
			}
			stage := strings.ToLower(strings.TrimSpace(step.Stage))
			switch {
			case stage == "provider":
				summary.ProviderSteps++
			case strings.HasPrefix(stage, "agent:"):
				summary.AgentSteps++
			}
			if strings.TrimSpace(step.Error) != "" {
				summary.ErrorSteps++
			}
			if pid := strings.TrimSpace(step.ProviderID); pid != "" {
				if _, ok := providerSet[pid]; !ok {
					providerSet[pid] = struct{}{}
					summary.ProviderIDs = append(summary.ProviderIDs, pid)
				}
			}
		}
		summary.TotalSteps = len(steps)
		if summary.ProviderSteps == 0 && strings.TrimSpace(final.Stage) == "provider" {
			summary.ProviderSteps = 1
		}
		if len(steps) > 0 {
			copied := make([]DecisionLogRecord, len(steps))
			copy(copied, steps)
			summary.Steps = copied
		}
		out = append(out, summary)
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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIntToBool(v sql.NullInt64) bool {
	return v.Valid && v.Int64 != 0
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

func normalizeSymbols(symbols []string) []string {
	if len(symbols) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, sym := range symbols {
		s := strings.ToUpper(strings.TrimSpace(sym))
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func normalizeTraceIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, id := range ids {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
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
