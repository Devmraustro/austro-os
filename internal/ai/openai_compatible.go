package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxResponseBytes bounds the response body read from an external AI endpoint.
// A real provider is CONFIGURATION_REQUIRED and operator-configured, but the
// boundary still guards against an oversized or misbehaving response by failing
// the operation instead of buffering unbounded memory.
const maxResponseBytes = 64 << 20 // 64 MiB

// OpenAICompatibleProvider is a real (CONFIGURATION_REQUIRED) Provider that
// calls an OpenAI-compatible HTTP endpoint. It implements the Provider port so
// callers never depend on it directly (Replaceability, Principles 6 and 8).
//
// The API key is held in memory and never written to any log or trace; it is
// only attached to the Authorization header of outbound requests. Scope
// (workspace id) is still required and validated at this boundary so no call is
// ever issued without a governing workspace.
type OpenAICompatibleProvider struct {
	baseURL string
	model   string
	apiKey  string
	client  *http.Client
}

// NewOpenAICompatibleProvider validates the CONFIGURATION_REQUIRED settings and
// returns the HTTP-backed provider. It fails fast on any missing setting rather
// than producing a provider that would fail mid-call.
func NewOpenAICompatibleProvider(cfg ProviderConfig) (*OpenAICompatibleProvider, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("%w: model is required", ErrProviderConfig)
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("%w: base url is required", ErrProviderConfig)
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("%w: api key is required", ErrProviderConfig)
	}
	client := cfg.HTTPClient
	if client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}
	return &OpenAICompatibleProvider{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		model:   cfg.Model,
		apiKey:  cfg.APIKey,
		client:  client,
	}, nil
}

// Complete calls POST {base}/chat/completions and returns the first choice text.
func (p *OpenAICompatibleProvider) Complete(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
	if err := validateScope(req.Scope); err != nil {
		return CompletionResult{}, err
	}
	payload := map[string]any{
		"model": p.model,
		"messages": []map[string]string{
			{"role": "user", "content": req.Instruction},
		},
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := p.post(ctx, "/chat/completions", payload, &out); err != nil {
		return CompletionResult{}, err
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return CompletionResult{}, fmt.Errorf("ai: provider returned no completion")
	}
	return CompletionResult{Text: out.Choices[0].Message.Content}, nil
}

// Embed calls POST {base}/embeddings and returns the first vector, truncated or
// padded to Dimensions as the request requires.
func (p *OpenAICompatibleProvider) Embed(ctx context.Context, req EmbedRequest) (EmbeddingResult, error) {
	if err := validateScope(req.Scope); err != nil {
		return EmbeddingResult{}, err
	}
	if strings.TrimSpace(req.Content) == "" {
		return EmbeddingResult{}, fmt.Errorf("%w: content is empty", ErrInvalidInput)
	}
	if req.Dimensions <= 0 {
		return EmbeddingResult{}, fmt.Errorf("%w: dimensions must be positive", ErrInvalidInput)
	}
	payload := map[string]any{
		"model": p.model,
		"input": req.Content,
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := p.post(ctx, "/embeddings", payload, &out); err != nil {
		return EmbeddingResult{}, err
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return EmbeddingResult{}, fmt.Errorf("ai: provider returned no embedding")
	}
	src := out.Data[0].Embedding
	values := make([]float32, req.Dimensions)
	for i := 0; i < req.Dimensions && i < len(src); i++ {
		values[i] = src[i]
	}
	return EmbeddingResult{Values: values, Dimensions: req.Dimensions}, nil
}

// Classify asks the model to pick one of the supplied categories and maps the
// response to a known category, defaulting to the first on an unrecognized or
// empty reply.
func (p *OpenAICompatibleProvider) Classify(ctx context.Context, req ClassificationRequest) (ClassificationResult, error) {
	if err := validateScope(req.Scope); err != nil {
		return ClassificationResult{}, err
	}
	if len(req.Categories) == 0 {
		return ClassificationResult{}, fmt.Errorf("%w: categories is empty", ErrInvalidInput)
	}
	if strings.TrimSpace(req.Input) == "" {
		return ClassificationResult{}, fmt.Errorf("%w: input is empty", ErrInvalidInput)
	}
	instruct := fmt.Sprintf(
		"Classify the input into exactly one of these categories and reply with only the category label: %s\n\nInput:\n%s",
		strings.Join(req.Categories, ", "), req.Input,
	)
	payload := map[string]any{
		"model": p.model,
		"messages": []map[string]string{
			{"role": "user", "content": instruct},
		},
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := p.post(ctx, "/chat/completions", payload, &out); err != nil {
		return ClassificationResult{}, err
	}
	if len(out.Choices) == 0 {
		return ClassificationResult{}, fmt.Errorf("ai: provider returned no classification")
	}
	label := strings.TrimSpace(out.Choices[0].Message.Content)
	for _, c := range req.Categories {
		if strings.EqualFold(label, c) {
			return ClassificationResult{Category: c}, nil
		}
	}
	return ClassificationResult{Category: req.Categories[0]}, nil
}

// post performs an authenticated JSON POST and decodes the response. The API
// key is sent via the Authorization header and is never included in any error
// message or returned value.
func (p *OpenAICompatibleProvider) post(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("ai: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("ai: provider request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("ai: read provider response: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("ai: provider response too large")
	}
	if resp.StatusCode != http.StatusOK {
		// Never include the key; surface only the status and a truncated body.
		return fmt.Errorf("ai: provider returned status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("ai: decode provider response: %w", err)
	}
	return nil
}

// ensure compile-time satisfaction of the Provider port.
var _ Provider = (*OpenAICompatibleProvider)(nil)
