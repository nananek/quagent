package guard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOllamaComplete(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("リクエストを読めない: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"content": `{"action":"deny","reason":"鍵"}`},
		})
	}))
	defer srv.Close()

	o := &Ollama{Endpoint: srv.URL, Model: "qwen2.5:3b"}
	out, err := o.Complete(context.Background(), "sys", "usr")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deny") {
		t.Errorf("out = %q", out)
	}
	if got["model"] != "qwen2.5:3b" || got["stream"] != false {
		t.Errorf("リクエストが違う: %v", got)
	}
	if _, ok := got["format"].(map[string]any); !ok {
		t.Errorf("format (JSON schema) が無い: %v", got["format"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Errorf("messages = %v", got["messages"])
	}
}

func TestOllamaError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "model not found"})
	}))
	defer srv.Close()
	o := &Ollama{Endpoint: srv.URL, Model: "nope"}
	if _, err := o.Complete(context.Background(), "s", "u"); err == nil {
		t.Fatal("エラーを返さなかった")
	}
}

func TestOpenAIComplete(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("リクエストを読めない: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": `{"action":"allow","reason":"ok"}`}}},
		})
	}))
	defer srv.Close()

	o := &OpenAI{Endpoint: srv.URL, Model: "local"}
	out, err := o.Complete(context.Background(), "sys", "usr")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "allow") {
		t.Errorf("out = %q", out)
	}
	if _, ok := got["response_format"].(map[string]any); !ok {
		t.Errorf("response_format が無い: %v", got["response_format"])
	}
}

func TestOllamaCompleteHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer srv.Close()
	o := &Ollama{Endpoint: srv.URL, Model: "m"}
	if _, err := o.Complete(context.Background(), "s", "u"); err == nil {
		t.Fatal("HTTP エラーを返さなかった")
	}
}

// response_format の json_schema に対応していないサーバーでは、json_object に落ちて
// 通った形を覚える (次からは最初から json_object で聞く)。
func TestOpenAIFallsBackWhenSchemaUnsupported(t *testing.T) {
	schemaSeen := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rf, _ := body["response_format"].(map[string]any)
		if rf != nil && rf["type"] == "json_schema" {
			schemaSeen++
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "response_format not supported"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": `{"action":"allow","reason":"ok"}`}}},
		})
	}))
	defer srv.Close()

	o := &OpenAI{Endpoint: srv.URL, Model: "m"}
	for i := 0; i < 2; i++ {
		out, err := o.Complete(context.Background(), "s", "u")
		if err != nil {
			t.Fatalf("%d 回目: %v", i+1, err)
		}
		if !strings.Contains(out, "allow") {
			t.Fatalf("%d 回目: out=%q", i+1, out)
		}
	}
	if schemaSeen != 1 {
		t.Errorf("json_schema を %d 回送った (落ちた形は覚えるはず)", schemaSeen)
	}
}
