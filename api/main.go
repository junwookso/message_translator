package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	runtime, err := loadRuntimeConfig(os.Getenv)
	if err != nil {
		return err
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	timeout, err := modelTimeout(os.Getenv("MODEL_TIMEOUT"))
	if err != nil {
		return err
	}
	inference, err := loadInferenceConfig(os.Getenv)
	if err != nil {
		return err
	}
	queueWait := inference.queueTimeout
	if inference.queue == 0 {
		queueWait = 0
	}
	if timeout > time.Duration(1<<63-1)-queueWait-20*time.Second {
		return fmt.Errorf("MODEL_TIMEOUT + API_QUEUE_TIMEOUT is too large")
	}
	requestWait := timeout + queueWait
	shutdownWait, err := shutdownTimeout(requestWait, os.Getenv("API_STOP_GRACE_PERIOD"))
	if err != nil {
		return err
	}
	modelName := os.Getenv("MODEL_NAME")
	if modelName == "" {
		modelName = "GEMMA_4_E4B"
	}
	promptVersion := envDefault("PROMPT_VERSION", "V1")
	prompt, err := loadPrompt(promptVersion)
	if err != nil {
		return err
	}
	modelVersion := envDefault("MODEL_VERSION", "GEMMA_4_E4B_Q4_0_V1")
	if !constantPattern.MatchString(modelVersion) {
		return fmt.Errorf("invalid MODEL_VERSION: use uppercase letters, digits, and underscores")
	}
	cacheEnabled, err := parseCacheEnabled(os.Getenv("CACHE_ENABLED"))
	if err != nil {
		return err
	}
	var model analyzer = sampleAnalyzer{}
	var cache *analysisCache
	if runtime.mode == "remote" {
		endpoint := runtime.endpoint
		model = &modelClient{endpoint: endpoint, model: modelName, prompt: prompt, client: &http.Client{Timeout: timeout}}
		identity, identityErr := runtime.cacheIdentity(model.(*modelClient), modelVersion, promptVersion)
		if identityErr != nil {
			return identityErr
		}
		cache, err = openConfiguredCache(cacheEnabled, envDefault("CACHE_DB_PATH", "./data/cache.db"), modelVersion, promptVersion, identity)
		if err != nil {
			log.Printf("cache initialization failed; continuing without cache: %v", err)
		} else if cache != nil {
			defer cache.Close()
		}
	}
	log.Printf("API configuration: env=%s model_mode=%s cache_enabled=%t", runtime.environment, runtime.mode, cache != nil)
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           newHandlerWithScheduler(model, cache, newInferenceScheduler(inference.inflight, inference.queue, inference.queueTimeout)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      requestWait + 10*time.Second,
		IdleTimeout:       60 * time.Second,
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", server.Addr, err)
	}
	defer listener.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, server, listener, shutdownWait)
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func serve(ctx context.Context, server *http.Server, listener net.Listener, shutdownWait time.Duration) error {
	requestsCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	server.BaseContext = func(net.Listener) context.Context { return requestsCtx }
	errCh := make(chan error, 1)
	go func() {
		log.Printf("API server started on %s", listener.Addr())
		errCh <- server.Serve(listener)
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		log.Printf("API server shutting down; waiting up to %s for active requests", shutdownWait)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownWait)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			cancelRequests()
			server.Close()
			return err
		}
		return nil
	}
}

func shutdownTimeout(modelWait time.Duration, graceValue string) (time.Duration, error) {
	grace := modelWait + 20*time.Second
	if graceValue != "" {
		var err error
		grace, err = time.ParseDuration(graceValue)
		if err != nil || grace < modelWait+20*time.Second {
			return 0, fmt.Errorf("API_STOP_GRACE_PERIOD must cover MODEL_TIMEOUT + queue wait + 20s")
		}
	}
	return grace - 10*time.Second, nil
}

func modelTimeout(value string) (time.Duration, error) {
	if value == "" {
		return 120 * time.Second, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout <= 0 || timeout > time.Duration(1<<63-1)-20*time.Second {
		return 0, fmt.Errorf("MODEL_TIMEOUT must be a positive duration with room for a 20s shutdown margin")
	}
	return timeout, nil
}

type equipmentMessageResponse struct {
	Identifier string  `json:"identifier"`
	Category   string  `json:"category"`
	Target     string  `json:"target"`
	Confidence float64 `json:"confidence"`
	Status     string  `json:"status"`
}

func newHandler(model analyzer) http.Handler {
	return newHandlerWithCache(model, nil)
}

func newHandlerWithCache(model analyzer, cache *analysisCache) http.Handler {
	return newHandlerWithScheduler(model, cache, newInferenceScheduler(1, 0, 5*time.Second))
}

func newHandlerWithScheduler(model analyzer, cache *analysisCache, scheduler *inferenceScheduler) http.Handler {
	if _, sample := model.(sampleAnalyzer); sample {
		cache = nil
	}
	return newAnalysisHandler(newAnalysisService(model, cache, scheduler))
}

func newAnalysisHandler(service *analysisService) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"message": "pong"})
	})
	// GET patterns also match HEAD in ServeMux; reject it before any cache or model work.
	methodNotAllowed := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
	mux.HandleFunc("HEAD /api/equipment/message", methodNotAllowed)
	mux.HandleFunc("/api/equipment/message", methodNotAllowed)
	mux.HandleFunc("GET /api/equipment/message", func(w http.ResponseWriter, r *http.Request) {
		message := r.URL.Query().Get("message")
		if len(message) > maxMessageBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "MESSAGE_TOO_LONG"})
			return
		}
		if strings.TrimSpace(message) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "MESSAGE_REQUIRED"})
			return
		}
		result, status, err := service.Analyze(r.Context(), message)
		if err != nil {
			var networkError net.Error
			switch {
			case errors.Is(err, context.Canceled):
				return
			case errors.Is(err, errModelBusy):
				w.Header().Set("Retry-After", "1")
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "MODEL_BUSY"})
			case errors.Is(err, errQueueTimeout):
				w.Header().Set("Retry-After", "1")
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "MODEL_QUEUE_TIMEOUT"})
			case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()):
				writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "MODEL_TIMEOUT"})
			default:
				log.Printf("model analysis failed: %v", err)
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "MODEL_ERROR"})
			}
			return
		}
		writeAnalysis(w, result, status)
	})
	return mux
}

func writeAnalysis(w http.ResponseWriter, result analysisResult, status string) {
	writeJSON(w, http.StatusOK, equipmentMessageResponse{
		Identifier: result.Identifier,
		Category:   result.Category,
		Target:     result.Target,
		Confidence: result.Confidence,
		Status:     status,
	})
}

const maxMessageBytes = 2048

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write JSON response: %v", err)
	}
}
