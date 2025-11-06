package ai

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "strconv"
    "strings"
    "time"
)
import "brale/internal/logger"

// 中文说明：
// OpenAIChatClient：兼容 OpenAI / DeepSeek / Qwen 的聊天补全接口（/v1/chat/completions）。

type OpenAIChatClient struct {
    BaseURL string
    APIKey  string
    Model   string
    Timeout time.Duration
    // 简易重试（用于 429/5xx）：若为 0 则默认重试 2 次
    MaxRetries int
    ExtraHeaders map[string]string
}

func (c *OpenAIChatClient) CallWithMessages(systemPrompt, userPrompt string) (string, error) {
    if c.Timeout == 0 {
        c.Timeout = 60 * time.Second
    }
    if c.MaxRetries < 0 { c.MaxRetries = 0 }
    maxRetries := c.MaxRetries
    if maxRetries == 0 { maxRetries = 2 }
    url := c.BaseURL
    if url == "" {
        url = "https://api.openai.com/v1"
    }
    url = url + "/chat/completions"

	// 构造消息
	messages := []map[string]string{}
	if systemPrompt != "" {
		messages = append(messages, map[string]string{"role": "system", "content": systemPrompt})
	}
	messages = append(messages, map[string]string{"role": "user", "content": userPrompt})

	body := map[string]any{"model": c.Model, "messages": messages, "temperature": 0.5}
	b, _ := json.Marshal(body)

    httpc := &http.Client{Timeout: c.Timeout}
    var lastErr error
    var out string
    for attempt := 0; attempt <= maxRetries; attempt++ {
        // 打印完整请求（仅首个尝试，debug 级别；授权头做掩码）
        if attempt == 0 {
            // 组装将要发送的头信息（与实际请求一致）
            hlog := map[string]string{"Content-Type": "application/json"}
            if c.APIKey != "" {
                // 掩码 Bearer 密钥，仅展示后 4 位
                tail := ""
                if len(c.APIKey) > 4 { tail = c.APIKey[len(c.APIKey)-4:] } else { tail = c.APIKey }
                hlog["Authorization"] = fmt.Sprintf("Bearer ****%s", tail)
            }
            for k, v := range c.ExtraHeaders {
                // 对可能包含敏感信息的头进行掩码
                lk := strings.ToLower(k)
                mv := v
                if strings.Contains(lk, "key") || strings.Contains(lk, "token") || strings.Contains(lk, "auth") {
                    if len(v) > 4 { mv = "****" + v[len(v)-4:] } else { mv = "****" }
                }
                hlog[k] = mv
            }
            logger.Debugf("[AI] 请求: POST %s, headers=%v, body=%s", url, hlog, string(b))
        }
        req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(b))
        req.Header.Set("Content-Type", "application/json")
        if c.APIKey != "" {
            req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.APIKey))
        }
        // 覆盖/补充自定义请求头（若配置中提供）
        if len(c.ExtraHeaders) > 0 {
            for k, v := range c.ExtraHeaders { req.Header.Set(k, v) }
        }

        resp, err := httpc.Do(req)
        if err != nil {
            lastErr = err
            break
        }
        if resp.StatusCode/100 == 2 {
            var r struct {
                Choices []struct {
                    Message struct {
                        Content string `json:"content"`
                    } `json:"message"`
                } `json:"choices"`
            }
            derr := json.NewDecoder(resp.Body).Decode(&r)
            resp.Body.Close()
            if derr != nil {
                lastErr = derr
                break
            }
            if len(r.Choices) == 0 {
                lastErr = fmt.Errorf("empty choices")
                break
            }
            out = r.Choices[0].Message.Content
            lastErr = nil
            return out, nil
        }
        // 非 2xx：尝试解析错误消息
        var eresp struct {
            Error struct {
                Message string      `json:"message"`
                Type    string      `json:"type"`
                Param   string      `json:"param"`
                Code    interface{} `json:"code"`
            } `json:"error"`
        }
        _ = json.NewDecoder(resp.Body).Decode(&eresp)
        resp.Body.Close()
        msg := strings.TrimSpace(eresp.Error.Message)
        if msg == "" { msg = resp.Status }
        // 对 429/5xx 进行有限重试（带 Retry-After 支持）
        if (resp.StatusCode == 429 || resp.StatusCode == 500 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504) && attempt < maxRetries {
            // 解析 Retry-After
            wait := time.Duration(0)
            if ra := resp.Header.Get("Retry-After"); ra != "" {
                if secs, perr := strconv.Atoi(ra); perr == nil { wait = time.Duration(secs) * time.Second }
            }
            if wait == 0 {
                // 基本指数退避：0.8s, 1.6s, 3.2s ...
                base := 800 * time.Millisecond
                wait = base << attempt
                if wait > 8*time.Second { wait = 8 * time.Second }
            }
            time.Sleep(wait)
            lastErr = fmt.Errorf("status=%d: %s", resp.StatusCode, msg)
            continue
        }
        lastErr = fmt.Errorf("status=%d: %s", resp.StatusCode, msg)
        break
    }
    return "", lastErr
}

// MCPModelProvider（重命名为 OpenAIModelProvider）：实现 ModelProvider
type OpenAIModelProvider struct {
	id      string
	enabled bool
	client  interface {
		CallWithMessages(systemPrompt, userPrompt string) (string, error)
	}
}

func NewOpenAIModelProvider(id string, enabled bool, client interface {
	CallWithMessages(string, string) (string, error)
}) *OpenAIModelProvider {
	return &OpenAIModelProvider{id: id, enabled: enabled, client: client}
}

func (p *OpenAIModelProvider) ID() string    { return p.id }
func (p *OpenAIModelProvider) Enabled() bool { return p.enabled }
func (p *OpenAIModelProvider) Call(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	return p.client.CallWithMessages(systemPrompt, userPrompt)
}
