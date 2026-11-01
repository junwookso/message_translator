package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const modelResponse = `{"identifier":"CHAMBER_HIGH_TEMPERATURE","category":"HIGH_TEMPERATURE","target":"CHAMBER_1","confidence":0.95}`

func TestModelIntegration(t *testing.T) {
	message := " 오븐 챔버 1 & 온도=높음? \n"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected model request: %s %s, %s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		var payload chatRequest
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("invalid request JSON: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(raw, &payload); err != nil || len(payload.Messages) != 2 || payload.Messages[1].Content != message {
			t.Errorf("message was not preserved: %v, %v", payload, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if payload.Model != "GEMMA_4_E4B" || payload.Stream || payload.MaxTokens != 256 || payload.ReasoningEffort != "none" {
			t.Errorf("unexpected generation options: %+v", payload)
		}
		if enabled, present := payload.ChatTemplateKwargs["enable_thinking"]; !present || enabled {
			t.Error("thinking must be explicitly disabled in chat template kwargs")
		}
		var budget struct {
			Tokens *int `json:"reasoning_budget_tokens"`
		}
		if err := json.Unmarshal(raw, &budget); err != nil || budget.Tokens == nil || *budget.Tokens != 0 {
			t.Error("request must explicitly set reasoning_budget_tokens to zero")
		}
		if payload.Messages[0].Role != "system" || payload.Messages[0].Content != mustPrompt(t, "V1") || payload.Messages[1].Role != "user" {
			t.Error("expected separate system instructions and user input")
		}
		schema, ok := payload.ResponseFormat["json_schema"].(map[string]any)
		if payload.ResponseFormat["type"] != "json_schema" || !ok || schema["schema"] == nil {
			t.Error("expected JSON schema response format")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, completionResponse(modelResponse, "stop"))
	}))
	defer server.Close()
	handler := newHandler(&modelClient{endpoint: server.URL + "/v1/chat/completions", model: "GEMMA_4_E4B", prompt: mustPrompt(t, "V1"), client: server.Client()})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/equipment/message?message="+url.QueryEscape(message), nil))
	want := strings.TrimSuffix(modelResponse, "}") + `,"status":"NEW"}`
	if response.Code != 200 || strings.TrimSpace(response.Body.String()) != want {
		t.Fatalf("response = %d %s, want 200 %s", response.Code, response.Body.String(), want)
	}
	for _, path := range []string{"/api/equipment/message", "/api/equipment/message?message=", "/api/equipment/message?message=%20", "/ping"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	if calls.Load() != 1 {
		t.Errorf("model called %d times, want 1", calls.Load())
	}
}

func TestModelFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{"server error", modelResponse, 500},
		{"unexpected success status", modelResponse, 201},
		{"invalid JSON", "not json", 200},
		{"trailing JSON", modelResponse + `{}`, 200},
		{"missing identifier", `{"category":"HIGH_TEMPERATURE","target":"CHAMBER_1","confidence":0}`, 200},
		{"missing category", `{"identifier":"ERROR","target":"CHAMBER_1","confidence":0}`, 200},
		{"missing target", `{"identifier":"ERROR","category":"HIGH_TEMPERATURE","confidence":0}`, 200},
		{"missing confidence", `{"identifier":"ERROR","category":"HIGH_TEMPERATURE","target":"CHAMBER_1"}`, 200},
		{"null confidence", strings.Replace(modelResponse, "0.95", "null", 1), 200},
		{"string confidence", strings.Replace(modelResponse, "0.95", `"0.95"`, 1), 200},
		{"empty identifier", strings.Replace(modelResponse, "CHAMBER_HIGH_TEMPERATURE", "", 1), 200},
		{"invalid category", strings.Replace(modelResponse, `"HIGH_TEMPERATURE"`, `"high temperature"`, 1), 200},
		{"invalid target", strings.Replace(modelResponse, "CHAMBER_1", "CHAMBER-1", 1), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, completionResponse(tc.body, "stop"))
			}))
			defer server.Close()
			assertModelError(t, &modelClient{endpoint: server.URL, client: server.Client()}, 502, "MODEL_ERROR")
		})
	}
}

func TestModelTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	assertModelError(t, &modelClient{endpoint: server.URL, client: &http.Client{Timeout: 50 * time.Millisecond}}, 504, "MODEL_TIMEOUT")
}

func TestModelConnectionFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := server.URL
	client := server.Client()
	server.Close()
	assertModelError(t, &modelClient{endpoint: endpoint, client: client}, 502, "MODEL_ERROR")
}

func TestModelRequestCancellation(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Consume the body so the server can observe the client disconnect.
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &modelClient{endpoint: server.URL, client: &http.Client{Timeout: time.Second}}
	done := make(chan error, 1)
	go func() {
		_, err := client.Analyze(ctx, "test")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("model request did not start")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("model request was not canceled")
	}
	if err := <-done; err == nil {
		t.Fatal("expected cancellation error")
	}
}

func assertModelError(t *testing.T, model analyzer, status int, code string) {
	t.Helper()
	response := httptest.NewRecorder()
	newHandler(model).ServeHTTP(response, httptest.NewRequest("GET", "/api/equipment/message?message=test", nil))
	want := `{"error":"` + code + `"}`
	if response.Code != status || strings.TrimSpace(response.Body.String()) != want {
		t.Fatalf("response = %d %s, want %d %s", response.Code, response.Body.String(), status, want)
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Error("error response must be JSON")
	}
}

func completionResponse(content, reason string) string {
	body, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"message":       map[string]string{"role": "assistant", "content": content},
		"finish_reason": reason,
	}}})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func TestCompletionFailures(t *testing.T) {
	for _, body := range []string{
		"not json", "{}", `{"choices":[]}`, `{"choices":[null]}`,
		completionResponse("", "stop"), completionResponse(modelResponse, "length"),
		completionResponse(modelResponse, "stop") + `{}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, body)
			}))
			defer server.Close()
			assertModelError(t, &modelClient{endpoint: server.URL, client: server.Client()}, 502, "MODEL_ERROR")
		})
	}
}

func TestModelTimeoutConfig(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{"", 120 * time.Second, false}, {"30s", 30 * time.Second, false},
		{"invalid", 0, true}, {"0s", 0, true}, {"-1s", 0, true},
	} {
		got, err := modelTimeout(tc.value)
		if (err != nil) != tc.invalid || got != tc.want {
			t.Errorf("modelTimeout(%q) = %v, %v", tc.value, got, err)
		}
	}
}
