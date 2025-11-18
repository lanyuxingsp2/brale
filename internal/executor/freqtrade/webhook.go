package freqtrade

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// WebhookMessage represents the JSON payload sent by freqtrade webhooks.
type WebhookMessage struct {
	Type        string       `json:"type"`
	TradeID     numericInt   `json:"trade_id"`
	Pair        string       `json:"pair"`
	Direction   string       `json:"direction"`
	Amount      numericFloat `json:"amount"`
	OrderRate   numericFloat `json:"order_rate"`
	OpenRate    numericFloat `json:"open_rate"`
	CloseRate   numericFloat `json:"close_rate"`
	StakeAmount numericFloat `json:"stake_amount"`
	Leverage    numericFloat `json:"leverage"`
	ExitReason  string       `json:"exit_reason"`
	Reason      string       `json:"reason"`
	ProfitRatio interface{}  `json:"profit_ratio"`
	IsFinalExit *bool        `json:"is_final_exit"`
	OpenDate    string       `json:"open_date"`
	CloseDate   string       `json:"close_date"`
}

// numericFloat unmarshals freqtrade numbers that may arrive as strings.
type numericFloat float64

func (f *numericFloat) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*f = 0
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*f = 0
			return nil
		}
		val, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		*f = numericFloat(val)
		return nil
	}
	var val float64
	if err := json.Unmarshal(data, &val); err != nil {
		return err
	}
	*f = numericFloat(val)
	return nil
}

// numericInt unmarshals freqtrade integers that may arrive as strings.
type numericInt int64

func (i *numericInt) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*i = 0
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*i = 0
			return nil
		}
		val, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		*i = numericInt(val)
		return nil
	}
	var val int64
	if err := json.Unmarshal(data, &val); err != nil {
		return err
	}
	*i = numericInt(val)
	return nil
}
