package main

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"
)

//go:embed prompts/*.txt
var prompts embed.FS

func loadPrompt(version string) (string, error) {
	return readPrompt(prompts, version)
}

func readPrompt(files fs.FS, version string) (string, error) {
	if !constantPattern.MatchString(version) {
		return "", fmt.Errorf("invalid PROMPT_VERSION: use uppercase letters, digits, and underscores")
	}
	data, err := fs.ReadFile(files, "prompts/"+version+".txt")
	if err != nil {
		return "", fmt.Errorf("load prompt %s: %w", version, err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("prompt %s is empty", version)
	}
	return string(data), nil
}
