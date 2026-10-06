package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// verdictSchema はローカル LLM に強制する出力の形。Ollama の structured outputs と、
// OpenAI 互換サーバーの json_schema 応答形式の両方で使う。evidence を載せ忘れると、
// strict な structured output ではモデルが引用を返せず、deny が必ず allow に落ちる
// (concreteVerdict は evidence の無い deny を通すため)。
var verdictSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"action":     map[string]any{"type": "string", "enum": []string{"allow", "deny"}},
		"reason":     map[string]any{"type": "string"},
		"evidence":   map[string]any{"type": "string"},
		"categories": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
	"required": []string{"action", "reason"},
}

// Ollama は Ollama の /api/chat を叩く Completer。
type Ollama struct {
	Endpoint string
	Model    string
	// NumCtx は文脈長 (トークン)。0 なら既定値。
	NumCtx int
	HTTP   *http.Client
}

func (o *Ollama) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return http.DefaultClient
}

func (o *Ollama) Complete(ctx context.Context, system, user string) (string, error) {
	numCtx := o.NumCtx
	if numCtx <= 0 {
		numCtx = defaultNumCtx
	}
	body, err := json.Marshal(map[string]any{
		"model":  o.Model,
		"stream": false,
		"format": verdictSchema,
		"options": map[string]any{
			"temperature": 0,
			"num_predict": 200,
			"num_ctx":     numCtx,
		},
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.Endpoint+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama に接続できない (%s): %w", o.Endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("ollama の応答を読めない (HTTP %d): %s", resp.StatusCode, clipBytes(string(raw), 200))
	}
	if out.Error != "" {
		return "", fmt.Errorf("ollama: %s", out.Error)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama が HTTP %d を返した", resp.StatusCode)
	}
	if strings.TrimSpace(out.Message.Content) == "" {
		return "", fmt.Errorf("ollama の応答が空")
	}
	return out.Message.Content, nil
}

// OpenAI は OpenAI 互換の /v1/chat/completions を叩く Completer (llama.cpp の
// llama-server など)。response_format の対応はビルドによって差があるので、対応して
// いなければ json_object、それも駄目なら付けずに再試行し、通った形を覚える。
type OpenAI struct {
	Endpoint string
	Model    string
	HTTP     *http.Client

	mu     sync.Mutex
	format string // "", "schema", "object", "none"
}

func (o *OpenAI) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return http.DefaultClient
}

func (o *OpenAI) Complete(ctx context.Context, system, user string) (string, error) {
	var lastErr error
	for _, f := range o.formats() {
		content, status, err := o.chat(ctx, system, user, f)
		if err == nil && status == http.StatusOK && strings.TrimSpace(content) != "" {
			o.setFormat(f)
			return content, nil
		}
		if err != nil {
			lastErr = err
		} else if strings.TrimSpace(content) == "" {
			lastErr = fmt.Errorf("ローカル LLM の応答が空")
		} else {
			lastErr = fmt.Errorf("ローカル LLM が HTTP %d を返した", status)
		}
		// 400 は response_format 非対応のことがあるので、次の形を試す
		if status != http.StatusBadRequest {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("ローカル LLM を呼べなかった")
	}
	return "", lastErr
}

// formats は試す response_format の順を返す。一度通った形があればそれだけを使う。
func (o *OpenAI) formats() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch o.format {
	case "schema", "object", "none":
		return []string{o.format}
	default:
		return []string{"schema", "object", "none"}
	}
}

func (o *OpenAI) setFormat(f string) {
	o.mu.Lock()
	o.format = f
	o.mu.Unlock()
}

// chat は response_format を f にして 1 回呼ぶ。f は "schema" / "object" / "none"。
func (o *OpenAI) chat(ctx context.Context, system, user, f string) (content string, status int, err error) {
	payload := map[string]any{
		"model":       o.Model,
		"temperature": 0,
		"max_tokens":  200,
		"stream":      false,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	switch f {
	case "schema":
		payload["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "verdict",
				"strict": true,
				"schema": verdictSchema,
			},
		}
	case "object":
		payload["response_format"] = map[string]any{"type": "json_object"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.Endpoint+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client().Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("OpenAI 互換サーバーに接続できない (%s): %w", o.Endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", resp.StatusCode, err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", resp.StatusCode, fmt.Errorf("応答を読めない (HTTP %d): %s", resp.StatusCode, clipBytes(string(raw), 200))
	}
	if out.Error.Message != "" {
		return "", resp.StatusCode, fmt.Errorf("ローカル LLM: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", resp.StatusCode, fmt.Errorf("ローカル LLM の応答が空 (HTTP %d)", resp.StatusCode)
	}
	return out.Choices[0].Message.Content, resp.StatusCode, nil
}
