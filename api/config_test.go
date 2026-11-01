package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		values  map[string]string
		invalid bool
	}{
		{"missing URL", nil, true},
		{"explicit sample", map[string]string{"MODEL_MODE": "sample"}, false},
		{"remote", map[string]string{"MODEL_API_URL": "http://model:8080/v1/chat/completions"}, false},
		{"prod missing URL", map[string]string{"APP_ENV": "prod"}, true},
		{"prod sample missing URL", map[string]string{"APP_ENV": "prod", "MODEL_MODE": "sample"}, true},
		{"prod sample", map[string]string{"APP_ENV": "prod", "MODEL_MODE": "sample", "MODEL_API_URL": "http://model"}, true},
		{"prod remote", map[string]string{"APP_ENV": "prod", "MODEL_API_URL": "http://model"}, false},
		{"unknown env", map[string]string{"APP_ENV": "production", "MODEL_MODE": "sample"}, true},
		{"unknown mode", map[string]string{"MODEL_MODE": "auto"}, true},
		{"mutable image", map[string]string{"MODEL_API_URL": "http://model", "LLAMA_CPP_IMAGE": "llama:latest"}, true},
		{"mutable revision", map[string]string{"MODEL_API_URL": "http://model", "MODEL_REVISION": "main"}, true},
		{"relative URL", map[string]string{"MODEL_API_URL": "model:8080"}, true},
		{"bad scheme", map[string]string{"MODEL_API_URL": "file:///model"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadRuntimeConfig(func(k string) string { return tc.values[k] })
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCacheConfigurationIsolation(t *testing.T) {
	config, _ := loadRuntimeConfig(func(k string) string {
		if k == "MODEL_API_URL" {
			return "http://model:8080/v1/chat/completions"
		}
		return ""
	})
	model := modelClient{model: "GEMMA_4_E4B", prompt: mustPrompt(t, "V1")}
	identity, err := config.cacheIdentity(&model, "M1", "V1")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cache.db")
	open := func(value string) *analysisCache {
		c, err := openConfiguredCache(true, path, "M1", "V1", value)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	result, _ := sampleAnalyzer{}.Analyze(context.Background(), "x")
	old := testCache(t, path, "M1", "V1")
	old.Put(context.Background(), "legacy", result)
	first := open(identity)
	if err := first.Put(context.Background(), "same", result); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := open(identity).Get(context.Background(), "same"); !found {
		t.Fatal("same configuration not reused")
	}
	if _, found, _ := first.Get(context.Background(), "legacy"); found {
		t.Fatal("unversioned legacy configuration reused")
	}
	for _, field := range []string{"repo", "revision", "file", "image", "endpoint", "prompt", "name"} {
		changed, m := config, model
		switch field {
		case "repo":
			changed.repo += "2"
		case "revision":
			changed.revision = "newrevision"
		case "file":
			changed.file = "new.gguf"
		case "image":
			changed.image += "2"
		case "endpoint":
			changed.endpoint += "2"
		case "prompt":
			m.prompt += "\nChanged"
		case "name":
			m.model += "2"
		}
		value, _ := changed.cacheIdentity(&m, "M1", "V1")
		if value == identity {
			t.Fatalf("%s not included", field)
		}
		if _, found, _ := open(value).Get(context.Background(), "same"); found {
			t.Fatalf("%s reused old cache", field)
		}
	}
	var stored string
	if err := first.db.QueryRow("SELECT configuration_json FROM cache_configurations WHERE fingerprint = ?", configurationHash(identity)).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != identity || !json.Valid([]byte(stored)) || !strings.Contains(stored, "max_tokens") {
		t.Fatal("configuration not recorded")
	}
}
