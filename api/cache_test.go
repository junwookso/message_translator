package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testCache(t *testing.T, path, model, prompt string) *analysisCache {
	t.Helper()
	c, err := openCache(path, model, prompt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestCacheEnabledConfig(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    bool
		invalid bool
	}{
		{"", true, false}, {"true", true, false}, {"false", false, false},
		{"invalid", false, true},
	} {
		got, err := parseCacheEnabled(tc.value)
		if got != tc.want || (err != nil) != tc.invalid {
			t.Errorf("parseCacheEnabled(%q) = %v, %v", tc.value, got, err)
		}
	}
}

func TestDisabledCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "cache.db")
	cache, err := openConfiguredCache(false, path, "M1", "V1")
	if err != nil || cache != nil {
		t.Fatalf("disabled cache: %v, %v", cache, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("disabled cache created DB directory")
	}
	// Existing entries must also be ignored and preserved while caching is disabled.
	existing := testCache(t, path, "M1", "V1")
	result, _ := sampleAnalyzer{}.Analyze(context.Background(), "test")
	if err := existing.Put(context.Background(), "test", result); err != nil {
		t.Fatal(err)
	}
	calls := 0
	model := analyzerFunc(func(context.Context, string) (analysisResult, error) { calls++; return result, nil })
	handler := newHandlerWithCache(model, cache)
	requestAnalysis(t, handler, "test", 200, "NEW")
	requestAnalysis(t, handler, "test", 200, "NEW")
	requestAnalysis(t, handler, "not_saved", 200, "NEW")
	if calls != 3 {
		t.Fatalf("model calls = %d, want 3", calls)
	}
	if _, found, err := existing.Get(context.Background(), "not_saved"); found || err != nil {
		t.Fatal("disabled cache saved new result")
	}
	if _, found, err := existing.Get(context.Background(), "test"); !found || err != nil {
		t.Fatal("disabled cache removed old result")
	}
	requestAnalysis(t, newHandlerWithCache(model, existing), "test", 200, "REUSED")
}

func requestAnalysis(t *testing.T, handler http.Handler, message string, code int, status string) equipmentMessageResponse {
	t.Helper()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/equipment/message?message="+url.QueryEscape(message), nil))
	if w.Code != code {
		t.Fatalf("response: %d %s; want %d", w.Code, w.Body.String(), code)
	}
	var result equipmentMessageResponse
	if status != "" {
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Status != status {
			t.Fatalf("status=%q, want %q", result.Status, status)
		}
	}
	return result
}

func TestCacheRoundTripAndVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, completionResponse(modelResponse, "stop"))
	}))
	defer server.Close()
	model := &modelClient{endpoint: server.URL, model: "TEST", prompt: mustPrompt(t, "V1"), client: server.Client()}
	c := testCache(t, path, "M1", "V1")
	handler := newHandlerWithCache(model, c)
	first := requestAnalysis(t, handler, "chamber 1 error", 200, "NEW")
	second := requestAnalysis(t, handler, "chamber 1 error", 200, "REUSED")
	second.Status = "NEW"
	if first != second {
		t.Fatal("cached result differs from original analysis")
	}
	if calls.Load() != 1 {
		t.Fatal("cache hit invoked model")
	}
	var encoded, created string
	if err := c.db.QueryRow("SELECT result_json, created_at FROM analysis_cache").Scan(&encoded, &created); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "status") || created == "" {
		t.Fatal("invalid persisted metadata")
	}
	c.Close()
	c = testCache(t, path, "M1", "V1")
	requestAnalysis(t, newHandlerWithCache(model, c), "chamber 1 error", 200, "REUSED")
	for _, message := range []string{"Chamber 1 error", " chamber 1 error", "chamber 2 error"} {
		requestAnalysis(t, newHandlerWithCache(model, c), message, 200, "NEW")
	}
	for _, version := range [][2]string{{"M2", "V1"}, {"M1", "V2"}} {
		cache := testCache(t, path, version[0], version[1])
		h := newHandlerWithCache(model, cache)
		requestAnalysis(t, h, "chamber 1 error", 200, "NEW")
		requestAnalysis(t, h, "chamber 1 error", 200, "REUSED")
	}
	if calls.Load() != 6 {
		t.Fatalf("model calls=%d, want 6", calls.Load())
	}
}

func TestCacheFailurePolicies(t *testing.T) {
	ctx := context.Background()
	result, _ := sampleAnalyzer{}.Analyze(ctx, "test")
	t.Run("closed database", func(t *testing.T) {
		cache := testCache(t, filepath.Join(t.TempDir(), "cache.db"), "M1", "V1")
		cache.Close()
		calls := 0
		model := analyzerFunc(func(context.Context, string) (analysisResult, error) { calls++; return result, nil })
		h := newHandlerWithCache(model, cache)
		requestAnalysis(t, h, "test", 200, "NEW")
		requestAnalysis(t, h, "test", 200, "NEW")
		if calls != 2 {
			t.Fatal("DB failure should not prevent model calls")
		}
	})
	t.Run("corrupt result", func(t *testing.T) {
		cache := testCache(t, filepath.Join(t.TempDir(), "cache.db"), "M1", "V1")
		for _, corrupt := range []string{"not json", `{"identifier":"ERROR"}`, strings.Replace(modelResponse, "CHAMBER_1", "chamber 1", 1)} {
			if err := cache.Put(ctx, "test", result); err != nil {
				t.Fatal(err)
			}
			if _, err := cache.db.Exec("UPDATE analysis_cache SET result_json = ?", corrupt); err != nil {
				t.Fatal(err)
			}
			model := analyzerFunc(func(context.Context, string) (analysisResult, error) { return result, nil })
			h := newHandlerWithCache(model, cache)
			requestAnalysis(t, h, "test", 200, "NEW")
			requestAnalysis(t, h, "test", 200, "REUSED")
		}
	})
	t.Run("model failure", func(t *testing.T) {
		cache := testCache(t, filepath.Join(t.TempDir(), "cache.db"), "M1", "V1")
		calls := 0
		h := newHandlerWithCache(analyzerFunc(func(context.Context, string) (analysisResult, error) {
			calls++
			return analysisResult{}, errors.New("model unavailable")
		}), cache)
		requestAnalysis(t, h, "test", 502, "")
		requestAnalysis(t, h, "test", 502, "")
		if _, found, err := cache.Get(ctx, "test"); err != nil || found || calls != 2 {
			t.Fatal("failed result was cached")
		}
	})
	t.Run("sample bypasses cache", func(t *testing.T) {
		cache := testCache(t, filepath.Join(t.TempDir(), "cache.db"), "M1", "V1")
		h := newHandlerWithCache(sampleAnalyzer{}, cache)
		requestAnalysis(t, h, "test", 200, "NEW")
		requestAnalysis(t, h, "test", 200, "NEW")
		if _, found, err := cache.Get(ctx, "test"); err != nil || found {
			t.Fatal("sample result was cached")
		}
	})
	t.Run("invalid DB file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.db")
		if err := os.WriteFile(path, []byte("invalid SQLite database"), 0600); err != nil {
			t.Fatal(err)
		}
		if c, err := openCache(path, "M1", "V1"); err == nil {
			c.Close()
			t.Fatal("expected initialization failure")
		}
	})
}

func TestCacheHitWhileModelBusy(t *testing.T) {
	cache := testCache(t, filepath.Join(t.TempDir(), "cache.db"), "M1", "V1")
	result, _ := sampleAnalyzer{}.Analyze(context.Background(), "cached")
	if err := cache.Put(context.Background(), "cached", result); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	h := newHandlerWithCache(analyzerFunc(func(ctx context.Context, message string) (analysisResult, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return analysisResult{}, ctx.Err()
		}
		return result, nil
	}), cache)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/equipment/message?message=uncached", nil).WithContext(ctx))
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("model did not start")
	}
	requestAnalysis(t, h, "cached", 200, "REUSED")
	requestAnalysis(t, h, "other", 429, "")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("model did not stop")
	}
}
