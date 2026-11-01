package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type analyzerFunc func(context.Context, string) (analysisResult, error)

func (f analyzerFunc) Analyze(ctx context.Context, message string) (analysisResult, error) {
	return f(ctx, message)
}

func TestMessageLengthLimit(t *testing.T) {
	var calls int
	handler := newHandler(analyzerFunc(func(ctx context.Context, message string) (analysisResult, error) {
		calls++
		return sampleAnalyzer{}.Analyze(ctx, message)
	}))
	for _, tc := range []struct {
		message string
		status  int
	}{
		{strings.Repeat("A", maxMessageBytes), 200},
		{strings.Repeat("A", maxMessageBytes+1), 413},
		{strings.Repeat("가", 682) + "AB", 200},
		{strings.Repeat("가", 683), 413},
	} {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest("GET", "/api/equipment/message?message="+url.QueryEscape(tc.message), nil))
		if r.Code != tc.status {
			t.Errorf("%d bytes: status = %d, want %d", len(tc.message), r.Code, tc.status)
		}
		if tc.status == 413 && strings.TrimSpace(r.Body.String()) != `{"error":"MESSAGE_TOO_LONG"}` {
			t.Errorf("unexpected length error: %s", r.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("model calls = %d, want 2", calls)
	}
}

func TestConcurrentAnalysisLimit(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	handler := newHandler(analyzerFunc(func(ctx context.Context, message string) (analysisResult, error) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			return sampleAnalyzer{}.Analyze(ctx, message)
		case <-ctx.Done():
			return analysisResult{}, ctx.Err()
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/equipment/message?message=test", nil).WithContext(ctx))
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first analysis did not start")
	}
	busy := httptest.NewRecorder()
	handler.ServeHTTP(busy, httptest.NewRequest("GET", "/api/equipment/message?message=second", nil))
	if busy.Code != 429 || strings.TrimSpace(busy.Body.String()) != `{"error":"MODEL_BUSY"}` || calls.Load() != 1 {
		t.Fatalf("unexpected busy response: %d %s; calls=%d", busy.Code, busy.Body.String(), calls.Load())
	}
	ping := httptest.NewRecorder()
	handler.ServeHTTP(ping, httptest.NewRequest("GET", "/ping", nil))
	if ping.Code != 200 {
		t.Fatal("ping should remain available while busy")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled analysis did not finish")
	}
	// Already-canceled requests must not start another model call.
	secondCtx, secondCancel := context.WithCancel(context.Background())
	secondCancel()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/equipment/message?message=third", nil).WithContext(secondCtx))
	if calls.Load() != 1 {
		t.Fatal("canceled request invoked the model")
	}
}

func TestShutdownTimeoutConfig(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{"", 130 * time.Second, false},
		{"140s", 130 * time.Second, false},
		{"3m", 170 * time.Second, false},
		{"139s", 0, true}, {"0s", 0, true}, {"invalid", 0, true},
	} {
		got, err := shutdownTimeout(120*time.Second, tc.value)
		if got != tc.want || (err != nil) != tc.invalid {
			t.Errorf("shutdownTimeout(%q) = %v, %v", tc.value, got, err)
		}
	}
}

func TestGracefulShutdown(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "finish active request", true: "cancel at deadline"}[forced], func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			started := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			canceled := make(chan struct{})
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				select {
				case <-release:
					io.WriteString(w, "completed")
				case <-r.Context().Done():
					close(canceled)
				}
			})}
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			defer server.Close()
			done := make(chan error, 1)
			go func() { done <- serve(ctx, server, listener, 100*time.Millisecond) }()
			clientDone := make(chan string, 1)
			go func() {
				client := &http.Client{Timeout: time.Second}
				resp, err := client.Get("http://" + listener.Addr().String())
				if err != nil {
					clientDone <- ""
					return
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				clientDone <- string(body)
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("request did not start")
			}
			stop()
			if !forced {
				select {
				case err := <-done:
					t.Fatalf("server exited before active request completed: %v", err)
				case <-time.After(10 * time.Millisecond):
				}
				release <- struct{}{}
			}
			select {
			case err := <-done:
				if forced && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("expected deadline, got %v", err)
				}
				if !forced && err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("shutdown did not finish")
			}
			if forced {
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("active request not canceled")
				}
			}
			if body := <-clientDone; !forced && body != "completed" {
				t.Fatalf("active request lost: %q", body)
			}
		})
	}
}
