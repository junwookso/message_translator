package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

type analysisResult struct {
	Identifier string  `json:"identifier"`
	Category   string  `json:"category"`
	Target     string  `json:"target"`
	Confidence float64 `json:"confidence"`
}

type analyzer interface {
	Analyze(context.Context, string) (analysisResult, error)
}

type sampleAnalyzer struct{}

func (sampleAnalyzer) Analyze(context.Context, string) (analysisResult, error) {
	return analysisResult{
		Identifier: "CHAMBER_HIGH_TEMPERATURE",
		Category:   "HIGH_TEMPERATURE",
		Target:     "CHAMBER_1",
		Confidence: 0,
	}, nil
}

type modelClient struct {
	endpoint string
	model    string
	prompt   string
	client   *http.Client
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model                 string          `json:"model"`
	Messages              []chatMessage   `json:"messages"`
	Stream                bool            `json:"stream"`
	MaxTokens             int             `json:"max_tokens"`
	ReasoningEffort       string          `json:"reasoning_effort"`
	ReasoningBudgetTokens int             `json:"reasoning_budget_tokens"`
	ChatTemplateKwargs    map[string]bool `json:"chat_template_kwargs"`
	ResponseFormat        map[string]any  `json:"response_format"`
}

func analysisSchema() map[string]any {
	text := map[string]any{"type": "string", "minLength": 1, "pattern": "^[A-Z0-9_]+$"}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"identifier": text, "category": text, "target": text,
			"confidence": map[string]any{"type": "number"},
		},
		"required":             []string{"identifier", "category", "target", "confidence"},
		"additionalProperties": false,
	}
}

var constantPattern = regexp.MustCompile(`^[A-Z0-9_]+$`)

func (m *modelClient) request(message string) chatRequest {
	return chatRequest{
		Model:    m.model,
		Messages: []chatMessage{{Role: "system", Content: m.prompt}, {Role: "user", Content: message}},
		Stream:   false, MaxTokens: 256, ReasoningEffort: "none",
		ChatTemplateKwargs: map[string]bool{"enable_thinking": false},
		// Enforce zero thinking tokens even when the model starts a reasoning channel.
		ReasoningBudgetTokens: 0,
		ResponseFormat: map[string]any{"type": "json_schema", "json_schema": map[string]any{
			"name": "equipment_analysis", "strict": true, "schema": analysisSchema(),
		}},
	}
}

func (m *modelClient) Analyze(ctx context.Context, message string) (analysisResult, error) {
	var result analysisResult
	body, err := json.Marshal(m.request(message))
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("model returned HTTP %d", resp.StatusCode)
	}
	var completion struct {
		Choices []struct {
			Message      chatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		} `json:"choices"`
	}
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&completion); err != nil {
		return result, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return result, fmt.Errorf("model response must contain one JSON value: %v", err)
	}
	if len(completion.Choices) == 0 {
		return result, fmt.Errorf("model returned no completion choices")
	}
	if completion.Choices[0].FinishReason != "stop" {
		return result, fmt.Errorf("model completion did not finish: finish_reason=%q", completion.Choices[0].FinishReason)
	}
	if strings.TrimSpace(completion.Choices[0].Message.Content) == "" {
		return result, fmt.Errorf("model returned empty content")
	}
	return decodeAnalysis(completion.Choices[0].Message.Content)
}

func decodeAnalysis(content string) (analysisResult, error) {
	var result analysisResult
	// Pointers distinguish missing/null fields from valid zero values.
	var payload struct {
		Identifier *string  `json:"identifier"`
		Category   *string  `json:"category"`
		Target     *string  `json:"target"`
		Confidence *float64 `json:"confidence"`
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	if err := decoder.Decode(&payload); err != nil {
		return result, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return result, fmt.Errorf("model response must contain one JSON value: %v", err)
	}
	if payload.Identifier == nil || payload.Category == nil || payload.Target == nil || payload.Confidence == nil {
		return result, fmt.Errorf("model response is missing required fields")
	}
	for _, value := range []string{*payload.Identifier, *payload.Category, *payload.Target} {
		if !constantPattern.MatchString(value) {
			return result, fmt.Errorf("model response contains an invalid string")
		}
	}
	return analysisResult{
		Identifier: *payload.Identifier,
		Category:   *payload.Category,
		Target:     *payload.Target,
		Confidence: *payload.Confidence,
	}, nil
}
