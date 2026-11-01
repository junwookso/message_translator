package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitForQueue(t *testing.T, s *inferenceScheduler, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := s.waiters.Len()
		s.mu.Unlock()
		if n == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue did not reach %d waiters", count)
}

type acquireResult struct {
	release func()
	err     error
}

func queuedAcquire(s *inferenceScheduler, ctx context.Context) <-chan acquireResult {
	done := make(chan acquireResult, 1)
	go func() { release, err := s.acquire(ctx); done <- acquireResult{release, err} }()
	return done
}

func receiveAcquire(t *testing.T, ch <-chan acquireResult) acquireResult {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(time.Second):
		t.Fatal("acquire did not finish")
	}
	return acquireResult{}
}

func TestSchedulerConcurrencyFIFOAndCapacity(t *testing.T) {
	s := newInferenceScheduler(2, 2, time.Second)
	first, err := s.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a := queuedAcquire(s, context.Background())
	waitForQueue(t, s, 1)
	b := queuedAcquire(s, context.Background())
	waitForQueue(t, s, 2)
	if _, err := s.acquire(context.Background()); !errors.Is(err, errModelBusy) {
		t.Fatalf("expected full queue, got %v", err)
	}
	first()
	next := receiveAcquire(t, a)
	if next.err != nil {
		t.Fatal(next.err)
	}
	select {
	case <-b:
		t.Fatal("second waiter overtook first")
	default:
	}
	second()
	last := receiveAcquire(t, b)
	if last.err != nil {
		t.Fatal(last.err)
	}
	next.release()
	last.release()
	if s.active != 0 || s.waiters.Len() != 0 {
		t.Fatal("slots or waiters leaked")
	}
}

func TestSchedulerTimeoutCancellationAndHandoff(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		s := newInferenceScheduler(1, 1, 10*time.Millisecond)
		release, _ := s.acquire(context.Background())
		defer release()
		if _, err := s.acquire(context.Background()); !errors.Is(err, errQueueTimeout) {
			t.Fatal(err)
		}
		waitForQueue(t, s, 0)
	})
	t.Run("cancellation", func(t *testing.T) {
		s := newInferenceScheduler(1, 1, time.Second)
		release, _ := s.acquire(context.Background())
		defer release()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := queuedAcquire(s, ctx)
		waitForQueue(t, s, 1)
		cancel()
		if result := receiveAcquire(t, done); !errors.Is(result.err, context.Canceled) {
			t.Fatal(result.err)
		}
		waitForQueue(t, s, 0)
	})
	t.Run("cancel during slot handoff", func(t *testing.T) {
		for i := 0; i < 30; i++ {
			s := newInferenceScheduler(1, 1, time.Second)
			release, _ := s.acquire(context.Background())
			ctx, cancel := context.WithCancel(context.Background())
			done := queuedAcquire(s, ctx)
			waitForQueue(t, s, 1)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); cancel() }()
			go func() { defer wg.Done(); release() }()
			result := receiveAcquire(t, done)
			wg.Wait()
			if result.err == nil {
				result.release()
			}
			if s.active != 0 || s.waiters.Len() != 0 {
				t.Fatal("slot leaked after cancellation")
			}
		}
	})
}

func TestQueuedHTTPAndCacheReuse(t *testing.T) {
	cache := testCache(t, filepath.Join(t.TempDir(), "cache.db"), "M1", "V1")
	result, _ := sampleAnalyzer{}.Analyze(context.Background(), "cached")
	if err := cache.Put(context.Background(), "cached", result); err != nil {
		t.Fatal(err)
	}
	s := newInferenceScheduler(1, 1, time.Second)
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	model := analyzerFunc(func(ctx context.Context, message string) (analysisResult, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return analysisResult{}, ctx.Err()
		}
		return result, nil
	})
	service := newAnalysisService(model, cache, s)
	handler := newAnalysisHandler(service)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := func() <-chan *httptest.ResponseRecorder {
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/equipment/message?message=same", nil).WithContext(ctx))
			done <- w
		}()
		return done
	}
	first := request()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("model did not start")
	}
	second := request()
	waitForParticipants(t, service, 2)
	requestAnalysis(t, handler, "cached", 200, "REUSED")
	requestAnalysis(t, handler, "overflow", 429, "")
	release <- struct{}{}
	for _, tc := range []struct {
		done   <-chan *httptest.ResponseRecorder
		status string
	}{{first, "NEW"}, {second, "REUSED"}} {
		select {
		case w := <-tc.done:
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"`+tc.status+`"`) {
				t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
			}
		case <-time.After(time.Second):
			t.Fatal("request did not finish")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("queued cache hit called model again")
	}
}

func TestQueueHTTPTimeout(t *testing.T) {
	s := newInferenceScheduler(1, 1, 10*time.Millisecond)
	release, _ := s.acquire(context.Background())
	defer release()
	h := newHandlerWithScheduler(sampleAnalyzer{}, nil, s)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/equipment/message?message=test", nil))
	if w.Code != 503 || strings.TrimSpace(w.Body.String()) != `{"error":"MODEL_QUEUE_TIMEOUT"}` || w.Header().Get("Retry-After") == "" {
		t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
	}
}

func TestInferenceConfig(t *testing.T) {
	defaults, err := loadInferenceConfig(func(string) string { return "" })
	if err != nil || defaults.parallel != 2 || defaults.inflight != 2 || defaults.queue != 4 || defaults.queueTimeout != 5*time.Second {
		t.Fatalf("invalid defaults: %+v, %v", defaults, err)
	}
	for _, values := range []map[string]string{
		{"MODEL_PARALLEL": "0"}, {"MODEL_PARALLEL": "524288"}, {"API_MAX_INFLIGHT": "0"},
		{"API_MAX_QUEUE": "-1"}, {"API_MAX_QUEUE": "x"}, {"API_QUEUE_TIMEOUT": "0s"},
		{"API_QUEUE_TIMEOUT": "invalid"}, {"MODEL_PARALLEL": "1", "API_MAX_INFLIGHT": "2"},
	} {
		if _, err := loadInferenceConfig(func(k string) string { return values[k] }); err == nil {
			t.Fatalf("accepted invalid config: %v", values)
		}
	}
	valid, err := loadInferenceConfig(func(k string) string {
		return map[string]string{"MODEL_PARALLEL": "1", "API_MAX_INFLIGHT": "1", "API_MAX_QUEUE": "0"}[k]
	})
	if err != nil || valid.queue != 0 {
		t.Fatalf("cannot disable queue: %+v, %v", valid, err)
	}
	if _, err := shutdownTimeout(125*time.Second, "140s"); err == nil {
		t.Fatal("shutdown ignored queue wait")
	}
	if _, err := shutdownTimeout(125*time.Second, "150s"); err != nil {
		t.Fatal(err)
	}
}
