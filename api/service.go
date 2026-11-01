package main

import (
	"context"
	"log"
	"sync"
)

// analysisService owns orchestration; HTTP handlers only translate requests/results.
type analysisService struct {
	model        analyzer
	cache        *analysisCache
	scheduler    *inferenceScheduler
	mu           sync.Mutex
	flights      map[string]*analysisFlight
	participants int
}

type analysisFlight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	result  analysisResult
	status  string
	err     error
}

func newAnalysisService(model analyzer, cache *analysisCache, scheduler *inferenceScheduler) *analysisService {
	return &analysisService{model: model, cache: cache, scheduler: scheduler, flights: make(map[string]*analysisFlight)}
}

func (s *analysisService) readCache(ctx context.Context, message string) (analysisResult, bool) {
	result, found, err := s.cache.Get(ctx, message)
	if err != nil {
		log.Printf("cache lookup failed; treating as miss: %v", err)
	}
	return result, found
}

func (s *analysisService) Analyze(ctx context.Context, message string) (analysisResult, string, error) {
	if err := ctx.Err(); err != nil {
		return analysisResult{}, "", err
	}
	if result, found := s.readCache(ctx, message); found {
		return result, "REUSED", nil
	}
	// With caching disabled every request must perform its own analysis.
	if s.cache == nil {
		return s.analyze(ctx, message)
	}
	s.mu.Lock()
	// Bound all admitted cache misses, including followers sharing an active job.
	if s.participants >= s.scheduler.maxActive+s.scheduler.maxQueue {
		s.mu.Unlock()
		return analysisResult{}, "", errModelBusy
	}
	flight, shared := s.flights[message]
	if !shared {
		workCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		flight = &analysisFlight{done: make(chan struct{}), cancel: cancel}
		s.flights[message] = flight
		go func() {
			defer cancel()
			result, status, err := s.analyze(workCtx, message)
			s.mu.Lock()
			flight.result, flight.status, flight.err = result, status, err
			if s.flights[message] == flight {
				delete(s.flights, message)
			}
			close(flight.done)
			s.mu.Unlock()
		}()
	}
	flight.waiters++
	s.participants++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		flight.waiters--
		s.participants--
		last := flight.waiters == 0
		if last {
			if s.flights[message] == flight {
				delete(s.flights, message)
			}
			flight.cancel()
		}
		s.mu.Unlock()
		// Keep the last HTTP handler alive until cancellation finishes the model work.
		// This also prevents graceful shutdown from closing SQLite under a worker.
		if last {
			<-flight.done
		}
	}()
	select {
	case <-ctx.Done():
		return analysisResult{}, "", ctx.Err()
	case <-flight.done:
		status := flight.status
		if shared && flight.err == nil {
			status = "REUSED"
		}
		return flight.result, status, flight.err
	}
}

func (s *analysisService) analyze(ctx context.Context, message string) (analysisResult, string, error) {
	release, err := s.scheduler.acquire(ctx)
	if err != nil {
		return analysisResult{}, "", err
	}
	defer release()
	if result, found := s.readCache(ctx, message); found {
		return result, "REUSED", nil
	}
	result, err := s.model.Analyze(ctx, message)
	if err != nil {
		return analysisResult{}, "", err
	}
	if err := s.cache.Put(ctx, message, result); err != nil {
		log.Printf("cache store failed; returning analysis result: %v", err)
	}
	return result, "NEW", nil
}
