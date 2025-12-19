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

type OpenAIChatClient struct {
	BaseURL      string
	APIKey       string
	Model        string
	Timeout      time.Duration
	MaxRetries   int
	ExtraHeaders map[string]string
}

func (c *OpenAIChatClient) Call(ctx context.Context, payload ChatPayload) (string, error) {
	ctx = ensureCtx(ctx)
	timeout := c.ensureTimeout()
	maxRetries := normalizeRetries(c.MaxRetries)
	url := c.chatCompletionsURL()

	bodyBytes := buildChatBodyBytes(c.Model, payload)
	logger.LogLLMPayload(c.Model, string(bodyBytes))

	httpc := &http.Client{Timeout: timeout}
	return c.doChatCompletions(ctx, httpc, url, bodyBytes, maxRetries)
}

func ensureCtx(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func normalizeRetries(v int) int {
	if v <= 0 {
		return 2
	}
	return v
}

func (c *OpenAIChatClient) ensureTimeout() time.Duration {
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	return c.Timeout
}

func (c *OpenAIChatClient) chatCompletionsURL() string {
	url := strings.TrimRight(c.BaseURL, "/")
	if url == "" {
		url = "https://api.openai.com/v1"
	}
	url = strings.TrimSuffix(url, "/chat/completions")
	return url + "/chat/completions"
}

func buildChatBodyBytes(model string, payload ChatPayload) []byte {
	messages := make([]map[string]any, 0, 3)
	if payload.System != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": payload.System,
		})
	}
	messages = append(messages, buildUserContent(payload))

	maxTokens := payload.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	body := map[string]any{
		"model":       model,
		"messages":    messages,
		"temperature": 0.4,
		"max_tokens":  maxTokens,
	}
	if payload.ExpectJSON {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	b, _ := json.Marshal(body)
	return b
}

func (c *OpenAIChatClient) doChatCompletions(ctx context.Context, httpc *http.Client, url string, body []byte, maxRetries int) (string, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt == 0 {
			logger.Debugf("[AI] 请求: POST %s headers=%v body=%s", url, redactHeaders(c.headersForLog()), string(body))
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		for k, v := range c.headers() {
			req.Header.Set(k, v)
		}
		resp, err := httpc.Do(req)
		if err != nil {
			lastErr = err
			break
		}

		if resp.StatusCode/100 == 2 {
			content, err := decodeChatContent(resp)
			if err != nil {
				lastErr = err
				break
			}
			return content, nil
		}

		msg := parseError(resp)
		lastErr = fmt.Errorf("status=%d: %s", resp.StatusCode, msg)
		if shouldRetry(resp.StatusCode) && attempt < maxRetries {
			wait := parseRetryAfter(resp.Header.Get("Retry-After"), attempt)
			time.Sleep(wait)
			continue
		}
		break
	}
	return "", lastErr
}

func decodeChatContent(resp *http.Response) (string, error) {
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			logger.Debugf("[AI] response body close failed: %v", cerr)
		}
	}()
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if len(r.Choices) == 0 {
		return "", fmt.Errorf("empty choices")
	}
	return r.Choices[0].Message.Content, nil
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
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			logger.Debugf("[AI] response body close failed: %v", cerr)
		}
	}()
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
