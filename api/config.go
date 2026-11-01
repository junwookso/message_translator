package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

const defaultModelRepo = "google/gemma-4-E4B-it-qat-q4_0-gguf"
const defaultModelRevision = "4b4a2c1d584be7264f87aac328a1bc739ce81b6c"
const defaultModelFile = "gemma-4-E4B_q4_0-it.gguf"
const defaultModelImage = "ghcr.io/ggml-org/llama.cpp@sha256:c50671c463e746cdccf2c6e77ffd6d77dca2e95290e54ca5ba5c7f524c33e234"

type runtimeConfig struct {
	environment, mode, endpoint string
	repo, revision, file, image string
}

func loadRuntimeConfig(getenv func(string) string) (runtimeConfig, error) {
	get := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	c := runtimeConfig{
		environment: get("APP_ENV", "dev"), mode: get("MODEL_MODE", "remote"), endpoint: getenv("MODEL_API_URL"),
		repo: get("MODEL_REPO", defaultModelRepo), revision: get("MODEL_REVISION", defaultModelRevision),
		file: get("MODEL_FILE", defaultModelFile), image: get("LLAMA_CPP_IMAGE", defaultModelImage),
	}
	if c.environment != "dev" && c.environment != "test" && c.environment != "prod" {
		return c, fmt.Errorf("APP_ENV must be dev, test, or prod")
	}
	if c.environment == "prod" && strings.TrimSpace(c.endpoint) == "" {
		return c, fmt.Errorf("MODEL_API_URL is required when APP_ENV=prod")
	}
	if c.mode != "remote" && c.mode != "sample" {
		return c, fmt.Errorf("MODEL_MODE must be remote or sample")
	}
	if c.environment == "prod" && c.mode == "sample" {
		return c, fmt.Errorf("MODEL_MODE=sample is not allowed when APP_ENV=prod")
	}
	if c.mode == "remote" {
		if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(c.revision) {
			return c, fmt.Errorf("MODEL_REVISION must be a full lowercase commit SHA")
		}
		if !regexp.MustCompile(`^.+@sha256:[a-f0-9]{64}$`).MatchString(c.image) {
			return c, fmt.Errorf("LLAMA_CPP_IMAGE must be pinned by sha256 digest")
		}
		u, err := url.Parse(c.endpoint)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
			return c, fmt.Errorf("MODEL_API_URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
		}
	}
	return c, nil
}

// The exact request template includes the prompt, JSON schema and generation options.
// Artifact coordinates describe the deployment; remote operators must keep them accurate.
func (c runtimeConfig) cacheIdentity(model *modelClient, modelVersion, promptVersion string) (string, error) {
	value := struct {
		ModelVersion  string      `json:"model_version"`
		PromptVersion string      `json:"prompt_version"`
		Endpoint      string      `json:"endpoint"`
		Repo          string      `json:"repo"`
		Revision      string      `json:"revision"`
		File          string      `json:"file"`
		Image         string      `json:"image"`
		Request       chatRequest `json:"request"`
	}{modelVersion, promptVersion, c.endpoint, c.repo, c.revision, c.file, c.image, model.request("")}
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

func configurationHash(value string) string { return fmt.Sprintf("%X", sha256.Sum256([]byte(value))) }
