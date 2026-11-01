package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func waitForParticipants(t *testing.T, s *analysisService, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		count := s.participants
		s.mu.Unlock()
		if count == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("participants did not reach %d", n)
}

type serviceResult struct {
	result analysisResult
	status string
	err    error
}

func callService(s *analysisService, ctx context.Context, message string) <-chan serviceResult {
	done := make(chan serviceResult, 1)
	go func() { r, status, err := s.Analyze(ctx, message); done <- serviceResult{r, status, err} }()
	return done
}
func receiveService(t *testing.T, ch <-chan serviceResult) serviceResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(time.Second):
		t.Fatal("analysis did not finish")
	}
	return serviceResult{}
}

func TestSharedAnalysisCancellation(t *testing.T) {
	for _, cancelLeader := range []bool{false, true} {
		t.Run(map[bool]string{false: "follower canceled", true: "leader canceled"}[cancelLeader], func(t *testing.T) {
			c := testCache(t, filepath.Join(t.TempDir(), "cache.db"), "M1", "V1")
			var calls atomic.Int32
			started, release := make(chan struct{}, 2), make(chan struct{})
			result, _ := sampleAnalyzer{}.Analyze(context.Background(), "x")
			model := analyzerFunc(func(ctx context.Context, _ string) (analysisResult, error) {
				calls.Add(1)
				started <- struct{}{}
				select {
				case <-ctx.Done():
					return analysisResult{}, ctx.Err()
				case <-release:
					return result, nil
				}
			})
			s := newAnalysisService(model, c, newInferenceScheduler(2, 1, time.Second))
			a, cancelA := context.WithCancel(context.Background())
			defer cancelA()
			b, cancelB := context.WithCancel(context.Background())
			defer cancelB()
			first := callService(s, a, "same")
			<-started
			second := callService(s, b, "same")
			waitForParticipants(t, s, 2)
			if calls.Load() != 1 {
				t.Fatal("duplicate inference")
			}
			if cancelLeader {
				cancelA()
				if !errors.Is(receiveService(t, first).err, context.Canceled) {
					t.Fatal("leader not canceled")
				}
			} else {
				cancelB()
				if !errors.Is(receiveService(t, second).err, context.Canceled) {
					t.Fatal("follower not canceled")
				}
			}
			close(release)
			remaining, want := first, "NEW"
			if cancelLeader {
				remaining, want = second, "REUSED"
			}
			got := receiveService(t, remaining)
			if got.err != nil || got.status != want || got.result != result {
				t.Fatalf("remaining: %+v", got)
			}
			if _, found, err := c.Get(context.Background(), "same"); !found || err != nil {
				t.Fatalf("cache missing: %v", err)
			}
		})
	}
}

func TestAllSharedWaitersCancel(t *testing.T) {
	c := testCache(t, filepath.Join(t.TempDir(), "cache.db"), "M1", "V1")
	started := make(chan struct{}, 2)
	var calls atomic.Int32
	model := analyzerFunc(func(ctx context.Context, _ string) (analysisResult, error) {
		calls.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		return analysisResult{}, ctx.Err()
	})
	scheduler := newInferenceScheduler(2, 1, time.Second)
	s := newAnalysisService(model, c, scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	first := callService(s, ctx, "same")
	<-started
	second := callService(s, ctx, "same")
	waitForParticipants(t, s, 2)
	cancel()
	receiveService(t, first)
	receiveService(t, second)
	s.mu.Lock()
	count, flights := s.participants, len(s.flights)
	s.mu.Unlock()
	scheduler.mu.Lock()
	active := scheduler.active
	scheduler.mu.Unlock()
	if count != 0 || flights != 0 || active != 0 || calls.Load() != 1 {
		t.Fatal("work or slots leaked")
	}
	if _, found, _ := c.Get(context.Background(), "same"); found {
		t.Fatal("canceled result cached")
	}
	// A canceled flight must not poison subsequent requests.
	retryCtx, retryCancel := context.WithCancel(context.Background())
	retry := callService(s, retryCtx, "same")
	<-started
	retryCancel()
	receiveService(t, retry)
	if calls.Load() != 2 {
		t.Fatal("retry did not start fresh")
	}
}

func TestSharedAdmissionAndFailure(t *testing.T) {
	c := testCache(t, filepath.Join(t.TempDir(), "cache.db"), "M1", "V1")
	release := make(chan struct{})
	var calls atomic.Int32
	failure := errors.New("model failed")
	model := analyzerFunc(func(context.Context, string) (analysisResult, error) {
		calls.Add(1)
		<-release
		return analysisResult{}, failure
	})
	s := newAnalysisService(model, c, newInferenceScheduler(2, 1, time.Second))
	first := callService(s, context.Background(), "same")
	waitForParticipants(t, s, 1)
	second := callService(s, context.Background(), "same")
	waitForParticipants(t, s, 2)
	third := callService(s, context.Background(), "same")
	waitForParticipants(t, s, 3)
	if _, _, err := s.Analyze(context.Background(), "same"); !errors.Is(err, errModelBusy) {
		t.Fatal("followers unbounded")
	}
	close(release)
	for _, ch := range []<-chan serviceResult{first, second, third} {
		if !errors.Is(receiveService(t, ch).err, failure) {
			t.Fatal("error not shared")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate inference")
	}
	s.Analyze(context.Background(), "same")
	if calls.Load() != 2 {
		t.Fatal("failure persisted")
	}
}
