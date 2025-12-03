package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"brale/internal/logger"
)

// OpenAIChatClient：兼容 OpenAI / DeepSeek / Qwen 的聊天补全接口（/v1/chat/completions）。
type OpenAIChatClient struct {
	BaseURL      string
	APIKey       string
	Model        string
	Timeout      time.Duration
	MaxRetries   int
	ExtraHeaders map[string]string
}

func (c *OpenAIChatClient) Call(ctx context.Context, payload ChatPayload) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	maxRetries := c.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 2
	}

	url := strings.TrimRight(c.BaseURL, "/")
	if url == "" {
		url = "https://api.openai.com/v1"
	}
	url = strings.TrimSuffix(url, "/chat/completions")
	url = url + "/chat/completions"

	messages := make([]map[string]any, 0, 3)
	if payload.System != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": payload.System,
		})
	}
	userContent := buildUserContent(payload)
	messages = append(messages, userContent)

	body := map[string]any{
		"model":       c.Model,
		"messages":    messages,
		"temperature": 0.4,
	}
	if payload.ExpectJSON {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	b, _ := json.Marshal(body)
	logger.LogLLMPayload(c.Model, string(b))

	httpc := &http.Client{Timeout: c.Timeout}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt == 0 {
			logger.Debugf("[AI] 请求: POST %s headers=%v body=%s", url, redactHeaders(c.headersForLog()), string(b))
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
		for k, v := range c.headers() {
			req.Header.Set(k, v)
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
			return r.Choices[0].Message.Content, nil
		}
		msg := parseError(resp)
		if shouldRetry(resp.StatusCode) && attempt < maxRetries {
			wait := parseRetryAfter(resp.Header.Get("Retry-After"), attempt)
			time.Sleep(wait)
			lastErr = fmt.Errorf("status=%d: %s", resp.StatusCode, msg)
			continue
		}
		lastErr = fmt.Errorf("status=%d: %s", resp.StatusCode, msg)
		break
	}
	return "", lastErr
}

func (c *OpenAIChatClient) headers() map[string]string {
	out := map[string]string{"Content-Type": "application/json"}
	if c.APIKey != "" {
		out["Authorization"] = fmt.Sprintf("Bearer %s", c.APIKey)
	}
	for k, v := range c.ExtraHeaders {
		out[k] = v
	}
	return out
}

func (c *OpenAIChatClient) headersForLog() map[string]string {
	out := map[string]string{}
	for k, v := range c.headers() {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "auth") || strings.Contains(lk, "key") || strings.Contains(lk, "token") {
			if len(v) > 4 {
				out[k] = "****" + v[len(v)-4:]
			} else {
				out[k] = "****"
			}
			continue
		}
		out[k] = v
	}
	return out
}

func buildUserContent(payload ChatPayload) map[string]any {
	if len(payload.Images) == 0 {
		return map[string]any{"role": "user", "content": payload.User}
	}
	content := make([]map[string]any, 0, len(payload.Images)*2+1)
	content = append(content, map[string]any{"type": "text", "text": payload.User})
	for _, img := range payload.Images {
		if strings.TrimSpace(img.DataURI) == "" {
			continue
		}
		entry := map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": strings.TrimSpace(img.DataURI),
			},
		}
		content = append(content, entry)
		if desc := strings.TrimSpace(img.Description); desc != "" {
			content = append(content, map[string]any{"type": "text", "text": desc})
		}
	}
	return map[string]any{"role": "user", "content": content}
}

func parseError(resp *http.Response) string {
	defer resp.Body.Close()
	var eresp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&eresp); err == nil && strings.TrimSpace(eresp.Error.Message) != "" {
		return eresp.Error.Message
	}
	return resp.Status
}

func shouldRetry(code int) bool {
	return code == 429 || code == 500 || code == 502 || code == 503 || code == 504
}

func parseRetryAfter(v string, attempt int) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	base := 800 * time.Millisecond
	wait := base << attempt
	if wait > 8*time.Second {
		wait = 8 * time.Second
	}
	return wait
}

func redactHeaders(headers map[string]string) map[string]string {
	return headers
}

// OpenAIModelProvider：实现 ModelProvider
type OpenAIModelProvider struct {
	id             string
	enabled        bool
	supportsVision bool
	expectJSON     bool
	client         interface {
		Call(ctx context.Context, payload ChatPayload) (string, error)
	}
}

func NewOpenAIModelProvider(id string, enabled bool, supportsVision, expectJSON bool, client interface {
	Call(context.Context, ChatPayload) (string, error)
}) *OpenAIModelProvider {
	return &OpenAIModelProvider{
		id:             id,
		enabled:        enabled,
		supportsVision: supportsVision,
		expectJSON:     expectJSON,
		client:         client,
	}
}

func (p *OpenAIModelProvider) ID() string           { return p.id }
func (p *OpenAIModelProvider) Enabled() bool        { return p.enabled }
func (p *OpenAIModelProvider) SupportsVision() bool { return p.supportsVision }
func (p *OpenAIModelProvider) ExpectsJSON() bool    { return p.expectJSON }
func (p *OpenAIModelProvider) Call(ctx context.Context, payload ChatPayload) (string, error) {
	return p.client.Call(ctx, payload)
}
