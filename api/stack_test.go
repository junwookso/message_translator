package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHeadDoesNotAnalyzeOrCache(t *testing.T) {
	cache := testCache(t, filepath.Join(t.TempDir(), "cache.db"), "M1", "V1")
	calls := 0
	handler := newHandlerWithCache(analyzerFunc(func(ctx context.Context, message string) (analysisResult, error) {
		calls++
		return sampleAnalyzer{}.Analyze(ctx, message)
	}), cache)
	for _, path := range []string{"/api/equipment/message?message=test", "/api/equipment/message"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("HEAD", path, nil))
		if w.Code != 405 || w.Header().Get("Allow") != "GET" || w.Body.Len() != 0 {
			t.Fatalf("unexpected HEAD response: %d %v %s", w.Code, w.Header(), w.Body.String())
		}
	}
	if calls != 0 {
		t.Fatal("HEAD invoked model")
	}
	if _, found, err := cache.Get(context.Background(), "test"); found || err != nil {
		t.Fatal("HEAD populated cache")
	}
	requestAnalysis(t, handler, "test", 200, "NEW")
	requestAnalysis(t, handler, "test", 200, "REUSED")
}

// A subprocess helper acts as the Compose executable for script tests.
func TestComposeCommandHelper(t *testing.T) {
	endpoint := os.Getenv("SIG_COMPOSE_TEST_CONTROL")
	if endpoint == "" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		os.Exit(2)
	}
	body, _ := json.Marshal(args[1:])
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(endpoint, "application/json", strings.NewReader(string(body)))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		os.Exit(1)
	}
	os.Exit(0)
}

func shutdownScript(t *testing.T, controlURL, engine string) *exec.Cmd {
	t.Helper()
	binDir := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\nexec \"$SIG_COMPOSE_TEST_BINARY\" -test.run=^TestComposeCommandHelper$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, engine), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "sh", "../scripts/down.sh")
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"), "CONTAINER_ENGINE="+engine,
		"SIG_COMPOSE_TEST_BINARY="+binary, "SIG_COMPOSE_TEST_CONTROL="+controlURL,
		"GORACE=atexit_sleep_ms=0")
	return cmd
}

func TestStackShutdownWaitsForAnalysis(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, completionResponse(modelResponse, "stop"))
	}))
	defer model.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	server := &http.Server{Handler: newHandler(&modelClient{endpoint: model.URL, prompt: mustPrompt(t, "V1"), client: model.Client()})}
	defer server.Close()
	apiDone := make(chan error, 1)
	go func() { apiDone <- serve(ctx, server, listener, 3*time.Second) }()
	response := make(chan *http.Response, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, _ := client.Get("http://" + listener.Addr().String() + "/api/equipment/message?message=test")
		response <- resp
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("analysis did not start")
	}
	stopping := make(chan struct{})
	commands := make(chan string, 2)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args []string
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			w.WriteHeader(400)
			return
		}
		command := strings.Join(args, " ")
		commands <- command
		switch command {
		case "compose stop api":
			stop()
			close(stopping)
			if err := <-apiDone; err != nil {
				w.WriteHeader(500)
				return
			}
		case "compose down":
			model.Close()
		default:
			w.WriteHeader(400)
		}
	}))
	defer control.Close()
	cmd := shutdownScript(t, control.URL, "podman")
	scriptDone := make(chan error, 1)
	go func() { scriptDone <- cmd.Run() }()
	select {
	case <-stopping:
	case <-time.After(2 * time.Second):
		t.Fatal("API stop was not requested")
	}
	if command := <-commands; command != "compose stop api" {
		t.Fatal("API must stop first")
	}
	select {
	case command := <-commands:
		t.Fatalf("model stopped before analysis completed: %s", command)
	case <-time.After(20 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case resp := <-response:
		if resp == nil {
			t.Fatal("active analysis connection was lost")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("active analysis returned %d", resp.StatusCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("analysis did not finish")
	}
	select {
	case err := <-scriptDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown script did not finish")
	}
	if command := <-commands; command != "compose down" {
		t.Fatal("down must follow API shutdown")
	}
}

func TestShutdownScriptStopsOnFailure(t *testing.T) {
	for _, engine := range []string{"podman", "docker"} {
		t.Run(engine, func(t *testing.T) {
			commands := make(chan string, 2)
			control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var args []string
				json.NewDecoder(r.Body).Decode(&args)
				commands <- strings.Join(args, " ")
				w.WriteHeader(500)
			}))
			defer control.Close()
			if err := shutdownScript(t, control.URL, engine).Run(); err == nil {
				t.Fatal("script ignored stop failure")
			}
			if command := <-commands; command != "compose stop api" {
				t.Fatal(command)
			}
			select {
			case command := <-commands:
				t.Fatalf("unexpected command after failure: %s", command)
			default:
			}
		})
	}
}

func TestModelStartScript(t *testing.T) {
	source, err := os.ReadFile("../model_service/start.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "start.sh")
	testSource := strings.Replace(string(source), "exec /app/llama-server", `exec "$SIG_TEST_LLAMA_BINARY"`, 1)
	if err := os.WriteFile(script, []byte(testSource), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		value, expected string
		invalid         bool
	}{
		{"", "2", false}, {"1", "1", false}, {"0002", "2", false},
		{"0", "", true}, {"-1", "", true}, {"two", "", true}, {"524288", "", true},
	} {
		cmd := exec.Command("sh", script, "--no-mmproj")
		cmd.Env = append(os.Environ(), "MODEL_PARALLEL="+tc.value, "SIG_TEST_LLAMA_BINARY="+binary, "LLAMA_CACHE="+dir, "MODEL_REPO="+defaultModelRepo, "MODEL_REVISION="+defaultModelRevision, "MODEL_FILE="+defaultModelFile)
		output, err := cmd.CombinedOutput()
		if tc.invalid {
			if err == nil {
				t.Errorf("accepted MODEL_PARALLEL=%q", tc.value)
			}
			continue
		}
		if err != nil {
			t.Fatalf("start failed: %v, %s", err, output)
		}
		ctxSize := "8192"
		if tc.expected == "1" {
			ctxSize = "4096"
		}
		want := "--model\n" + filepath.Join(dir, defaultModelRepo, defaultModelRevision, defaultModelFile) + "\n--model-url\nhttps://huggingface.co/" + defaultModelRepo + "/resolve/" + defaultModelRevision + "/" + defaultModelFile + "\n--ctx-size\n" + ctxSize + "\n--parallel\n" + tc.expected + "\n--no-mmproj\n"
		if string(output) != want {
			t.Errorf("args=%q, want %q", output, want)
		}
	}
}
