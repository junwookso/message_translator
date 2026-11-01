package main

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

var (
	errModelBusy    = errors.New("model queue is full")
	errQueueTimeout = errors.New("model queue wait timed out")
)

type inferenceConfig struct {
	parallel, inflight, queue int
	queueTimeout              time.Duration
}

func loadInferenceConfig(getenv func(string) string) (inferenceConfig, error) {
	var config inferenceConfig
	for _, setting := range []struct {
		name          string
		fallback, min int
		target        *int
	}{
		{"MODEL_PARALLEL", 2, 1, &config.parallel},
		{"API_MAX_INFLIGHT", 2, 1, &config.inflight},
		{"API_MAX_QUEUE", 4, 0, &config.queue},
	} {
		value := getenv(setting.name)
		if value == "" {
			*setting.target = setting.fallback
			continue
		}
		number, err := strconv.Atoi(value)
		if err != nil || number < setting.min {
			return config, fmt.Errorf("%s must be an integer >= %d", setting.name, setting.min)
		}
		*setting.target = number
	}
	if config.inflight > config.parallel {
		return config, fmt.Errorf("API_MAX_INFLIGHT must not exceed MODEL_PARALLEL")
	}
	if config.parallel > 524287 {
		return config, fmt.Errorf("MODEL_PARALLEL exceeds the context size limit (524287)")
	}
	config.queueTimeout = 5 * time.Second
	if value := getenv("API_QUEUE_TIMEOUT"); value != "" {
		var err error
		config.queueTimeout, err = time.ParseDuration(value)
		if err != nil || config.queueTimeout <= 0 || config.queueTimeout > time.Duration(1<<63-1)-20*time.Second {
			return config, fmt.Errorf("API_QUEUE_TIMEOUT must be a positive duration")
		}
	}
	return config, nil
}

type inferenceWaiter struct {
	ready   chan struct{}
	granted bool
}

type inferenceScheduler struct {
	mu                          sync.Mutex
	active, maxActive, maxQueue int
	waiters                     list.List
	waitTimeout                 time.Duration
}

func newInferenceScheduler(inflight, queue int, timeout time.Duration) *inferenceScheduler {
	return &inferenceScheduler{maxActive: inflight, maxQueue: queue, waitTimeout: timeout}
}

func (s *inferenceScheduler) acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.active < s.maxActive {
		s.active++
		s.mu.Unlock()
		return s.release, nil
	}
	if s.waiters.Len() >= s.maxQueue {
		s.mu.Unlock()
		return nil, errModelBusy
	}
	waiter := &inferenceWaiter{ready: make(chan struct{})}
	element := s.waiters.PushBack(waiter)
	s.mu.Unlock()
	waitCtx, cancel := context.WithTimeout(ctx, s.waitTimeout)
	defer cancel()
	select {
	case <-waiter.ready:
		if err := waitCtx.Err(); err == nil {
			return s.release, nil
		}
		// The slot was granted as the deadline/cancellation fired; return it to the next waiter.
		s.release()
	case <-waitCtx.Done():
		s.mu.Lock()
		granted := waiter.granted
		if !granted {
			s.waiters.Remove(element)
		}
		s.mu.Unlock()
		if granted {
			s.release()
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, errQueueTimeout
}

func (s *inferenceScheduler) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if element := s.waiters.Front(); element != nil {
		s.waiters.Remove(element)
		waiter := element.Value.(*inferenceWaiter)
		waiter.granted = true
		close(waiter.ready)
		return
	}
	s.active--
}
