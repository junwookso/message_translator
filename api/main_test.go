package main

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAPI(t *testing.T) {
	handler := newHandler(sampleAnalyzer{})
	const sample = `{"identifier":"CHAMBER_HIGH_TEMPERATURE","category":"HIGH_TEMPERATURE","target":"CHAMBER_1","confidence":0,"status":"NEW"}`
	const required = `{"error":"MESSAGE_REQUIRED"}`
	for _, tc := range []struct {
		name, method, path string
		status             int
		body               string
	}{
		{"message", "GET", "/api/equipment/message?message=motor%20error", 200, sample},
		{"unicode", "GET", "/api/equipment/message?message=" + url.QueryEscape("모터 오류 & 온도=높음?"), 200, sample},
		{"missing", "GET", "/api/equipment/message", 400, required},
		{"empty", "GET", "/api/equipment/message?message=", 400, required},
		{"whitespace", "GET", "/api/equipment/message?message=" + url.QueryEscape(" \t\n"), 400, required},
		{"unsupported method", "POST", "/api/equipment/message?message=test", 405, ""},
		{"head analysis", "HEAD", "/api/equipment/message?message=test", 405, ""},
		{"ping", "GET", "/ping", 200, `{"message":"pong"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d", response.Code, tc.status)
			}
			if tc.body != "" {
				if got := strings.TrimSpace(response.Body.String()); got != tc.body {
					t.Errorf("body = %s, want %s", got, tc.body)
				}
				if got := response.Header().Get("Content-Type"); got != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", got)
				}
			}
		})
	}
}
