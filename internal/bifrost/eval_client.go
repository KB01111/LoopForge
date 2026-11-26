package bifrost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// EvalClient calls Bifrost's OpenAI-compatible /v1/chat/completions endpoint
// directly so run evals and holdout gates can execute without requiring a CLI.
type EvalClient struct {
	BaseURL    string
	VirtualKey string
	HTTPClient *http.Client
}

// NewEvalClient creates a Bifrost inference client for eval and probe calls.
func NewEvalClient(baseURL, virtualKey string) *EvalClient {
	return &EvalClient{
		BaseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		VirtualKey: strings.TrimSpace(virtualKey),
		HTTPClient: &http.Client{Timeout: 3 * time.Minute},
	}
}

// EvalResponse captures the model output and token accounting from a completion call.
type EvalResponse struct {
	Content      string `json:"content"`
	Model        string `json:"model,omitempty"`
	InputTokens  int64  `json:"inputTokens,omitempty"`
	OutputTokens int64  `json:"outputTokens,omitempty"`
	TotalTokens  int64  `json:"totalTokens,omitempty"`
}

// Evaluate invokes POST /v1/chat/completions through Bifrost.
func (c *EvalClient) Evaluate(ctx context.Context, model, systemPrompt, userPrompt string) (EvalResponse, error) {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" {
		return EvalResponse{}, errors.New("bifrost base URL is not configured")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return EvalResponse{}, errors.New("eval model is required")
	}
	messages := make([]map[string]string, 0, 2)
	if strings.TrimSpace(systemPrompt) != "" {
		messages = append(messages, map[string]string{"role": "system", "content": systemPrompt})
	}
	messages = append(messages, map[string]string{"role": "user", "content": userPrompt})

	reqBody := map[string]any{
		"model":       model,
		"messages":    messages,
		"temperature": 0,
	}
	return c.chatCompletion(ctx, reqBody)
}

// TestProbeResult describes an end-to-end 1-token connectivity test against Bifrost.
type TestProbeResult struct {
	OK        bool   `json:"ok"`
	Model     string `json:"model"`
	Reply     string `json:"reply,omitempty"`
	LatencyMs int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

// TestCompletion runs a minimal chat completion against model to verify live routing.
func (c *EvalClient) TestCompletion(ctx context.Context, model string) (TestProbeResult, error) {
	start := time.Now()
	resp, err := c.chatCompletion(ctx, map[string]any{
		"model": strings.TrimSpace(model),
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with OK."},
		},
		"max_tokens":  8,
		"temperature": 0,
	})
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return TestProbeResult{OK: false, Model: model, LatencyMs: latency, Error: err.Error()}, err
	}
	return TestProbeResult{
		OK:        true,
		Model:     firstNonEmptyStr(resp.Model, model),
		Reply:     strings.TrimSpace(resp.Content),
		LatencyMs: latency,
	}, nil
}

func (c *EvalClient) chatCompletion(ctx context.Context, payload map[string]any) (EvalResponse, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return EvalResponse{}, err
	}
	u := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return EvalResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-loopforge-tier", "eval")
	if c.VirtualKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.VirtualKey)
		if strings.HasPrefix(c.VirtualKey, "sk-bf-") {
			req.Header.Set("x-bf-vk", c.VirtualKey)
		}
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return EvalResponse{}, fmt.Errorf("bifrost completion request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, defaultHTTPMaxLen))
	if err != nil {
		return EvalResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 512 {
			msg = msg[:512]
		}
		return EvalResponse{}, &HTTPStatusError{
			Method:     http.MethodPost,
			Path:       "/v1/chat/completions",
			StatusCode: resp.StatusCode,
			Body:       msg,
		}
	}
	var parsed struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return EvalResponse{}, fmt.Errorf("decode bifrost completion response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return EvalResponse{}, errors.New("bifrost completion returned no choices")
	}
	return EvalResponse{
		Content:      parsed.Choices[0].Message.Content,
		Model:        parsed.Model,
		InputTokens:  parsed.Usage.PromptTokens,
		OutputTokens: parsed.Usage.CompletionTokens,
		TotalTokens:  parsed.Usage.TotalTokens,
	}, nil
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
