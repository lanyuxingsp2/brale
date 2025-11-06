package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// 中文说明：
// Telegram 通知器：在开仓决策触发时，将关键信息推送至指定群/频道。

type Telegram struct {
	BotToken string
	ChatID   string
	Client   *http.Client
}

func NewTelegram(botToken, chatID string) *Telegram {
	return &Telegram{BotToken: botToken, ChatID: chatID, Client: &http.Client{Timeout: 15 * time.Second}}
}

// SendText 发送文本消息（带最多 3 次重试）
func (t *Telegram) SendText(text string) error {
	if t.BotToken == "" || t.ChatID == "" {
		return fmt.Errorf("Telegram 配置不完整")
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.BotToken)

	payload := map[string]any{
		"chat_id":    t.ChatID,
		"text":       text,
		"parse_mode": "Markdown",
	}
	body, _ := json.Marshal(payload)

	var lastErr error
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := t.Client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(i+1) * time.Second)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			return nil
		}
		lastErr = fmt.Errorf("telegram status=%d", resp.StatusCode)
		time.Sleep(time.Duration(i+1) * time.Second)
	}
	return lastErr
}
