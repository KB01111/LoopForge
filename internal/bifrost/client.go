// Package bifrost integrates Maxim AI's Bifrost LLM router/gateway with LoopForge.
package bifrost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultPort       = 8091
	DefaultBaseURL    = "http://127.0.0.1:8091"
	defaultHTTPMaxLen = 4 << 20
)

// Client is a lightweight HTTP adapter for Bifrost's inference and control-plane APIs.
type Client struct {
	BaseURL    string
	AdminToken string
	VirtualKey string
	HTTPClient *http.Client
}

// NewClient creates a Bifrost HTTP client with sane timeouts.
func NewClient(baseURL, adminToken, virtualKey string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		AdminToken: strings.TrimSpace(adminToken),
		VirtualKey: strings.TrimSpace(virtualKey),
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (c *Client) endpoint(path string) (string, error) {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" {
		return "", errors.New("bifrost base URL is not configured")
	}
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return "", fmt.Errorf("invalid bifrost base URL %q", c.BaseURL)
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path, nil
}

// HealthStatus is the normalized result of GET /health.
type HealthStatus struct {
	OK        bool   `json:"ok"`
	Status    string `json:"status"`
	Version   string `json:"version,omitempty"`
	LatencyMs int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

// Health probes Bifrost's public /health endpoint.
func (c *Client) Health(ctx context.Context) (HealthStatus, error) {
	u, err := c.endpoint("/health")
	if err != nil {
		return HealthStatus{OK: false, Status: "unconfigured", Error: err.Error()}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return HealthStatus{OK: false, Status: "error", Error: err.Error()}, err
	}
	start := time.Now()
	resp, err := c.httpClient().Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return HealthStatus{OK: false, Status: "unreachable", LatencyMs: latency, Error: err.Error()}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		return HealthStatus{OK: false, Status: "unhealthy", LatencyMs: latency, Error: msg}, errors.New(msg)
	}
	out := HealthStatus{OK: true, Status: "ok", LatencyMs: latency}
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		if s, ok := payload["status"].(string); ok && strings.TrimSpace(s) != "" {
			out.Status = strings.TrimSpace(s)
		}
		if v, ok := payload["version"].(string); ok && strings.TrimSpace(v) != "" {
			out.Version = strings.TrimSpace(v)
		}
	}
	return out, nil
}

// ModelEntry is a normalized model returned by GET /v1/models.
type ModelEntry struct {
	ID       string `json:"id"`
	Provider string `json:"provider,omitempty"`
	OwnedBy  string `json:"ownedBy,omitempty"`
}

// ListModels fetches available models from Bifrost's OpenAI-compatible /v1/models.
func (c *Client) ListModels(ctx context.Context) ([]ModelEntry, error) {
	body, err := c.doJSON(ctx, http.MethodGet, "/v1/models", nil, false)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode bifrost /v1/models: %w", err)
	}
	out := make([]ModelEntry, 0, len(payload.Data))
	seen := make(map[string]bool, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		provider := strings.TrimSpace(item.OwnedBy)
		if prefix, _, ok := strings.Cut(id, "/"); ok && strings.TrimSpace(prefix) != "" {
			provider = strings.TrimSpace(prefix)
		}
		out = append(out, ModelEntry{
			ID:       id,
			Provider: provider,
			OwnedBy:  strings.TrimSpace(item.OwnedBy),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ProviderKeySummary represents a sanitized upstream provider key.
type ProviderKeySummary struct {
	Name       string   `json:"name"`
	Models     []string `json:"models,omitempty"`
	Weight     float64  `json:"weight,omitempty"`
	Configured bool     `json:"configured"`
	Preview    string   `json:"preview,omitempty"`
}

// ProviderSummary represents a sanitized upstream provider configured in Bifrost.
type ProviderSummary struct {
	Name          string               `json:"name"`
	KeyCount      int                  `json:"keyCount"`
	Keys          []ProviderKeySummary `json:"keys,omitempty"`
	Models        []string             `json:"models,omitempty"`
	BaseURL       string               `json:"baseUrl,omitempty"`
	NetworkConfig map[string]any       `json:"networkConfig,omitempty"`
	Status        string               `json:"status,omitempty"`
}

// ProviderKeyInput is a write payload for configuring a Bifrost provider key.
type ProviderKeyInput struct {
	Name   string   `json:"name"`
	Value  string   `json:"value"`
	Models []string `json:"models,omitempty"`
	Weight float64  `json:"weight,omitempty"`
}

// ProviderUpsertRequest configures or updates an upstream provider in Bifrost.
type ProviderUpsertRequest struct {
	Keys                []ProviderKeyInput `json:"keys,omitempty"`
	NetworkConfig       map[string]any     `json:"network_config,omitempty"`
	CustomProviderConfig map[string]any    `json:"custom_provider_config,omitempty"`
}

// ListProviders reads configured providers from GET /api/providers and scrubs raw API keys.
func (c *Client) ListProviders(ctx context.Context) ([]ProviderSummary, error) {
	body, err := c.doJSON(ctx, http.MethodGet, "/api/providers", nil, true)
	if err != nil {
		return nil, err
	}
	return decodeProvidersPayload(body)
}

// UpsertProvider creates or updates an upstream provider via PUT /api/providers/{name}
// (falling back to POST /api/providers when the gateway expects collection POST).
func (c *Client) UpsertProvider(ctx context.Context, name string, req ProviderUpsertRequest) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("provider name is required")
	}
	_, err := c.doJSON(ctx, http.MethodPut, "/api/providers/"+url.PathEscape(name), req, true)
	if err == nil {
		return nil
	}
	var httpErr *HTTPStatusError
	if errors.As(err, &httpErr) && (httpErr.StatusCode == http.StatusNotFound || httpErr.StatusCode == http.StatusMethodNotAllowed) {
		payload := map[string]any{
			"provider": name,
			"keys":     req.Keys,
		}
		if len(req.NetworkConfig) > 0 {
			payload["network_config"] = req.NetworkConfig
		}
		if len(req.CustomProviderConfig) > 0 {
			payload["custom_provider_config"] = req.CustomProviderConfig
		}
		_, err = c.doJSON(ctx, http.MethodPost, "/api/providers", payload, true)
	}
	return err
}

// BudgetSpec describes a Bifrost spend budget window.
type BudgetSpec struct {
	ID            string  `json:"id,omitempty"`
	MaxLimit      float64 `json:"max_limit"`
	CurrentUsage  float64 `json:"current_usage,omitempty"`
	ResetDuration string  `json:"reset_duration,omitempty"`
}

// VirtualKeyProviderConfig scopes a virtual key to a specific provider/model allowlist.
type VirtualKeyProviderConfig struct {
	Provider      string   `json:"provider"`
	AllowedModels []string `json:"allowed_models,omitempty"`
	Weight        float64  `json:"weight,omitempty"`
}

// FallbackTarget describes one step in a Bifrost automatic fallback chain.
type FallbackTarget struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// VirtualKey is a Bifrost governance virtual key.
type VirtualKey struct {
	ID              string                     `json:"id,omitempty"`
	Name            string                     `json:"name"`
	Value           string                     `json:"value,omitempty"`
	ValuePreview    string                     `json:"valuePreview,omitempty"`
	IsActive        bool                       `json:"is_active"`
	Description     string                     `json:"description,omitempty"`
	Budgets         []BudgetSpec               `json:"budgets,omitempty"`
	Budget          *BudgetSpec                `json:"budget,omitempty"`
	ProviderConfigs []VirtualKeyProviderConfig `json:"provider_configs,omitempty"`
	Fallbacks       []FallbackTarget           `json:"fallbacks,omitempty"`
}

// VirtualKeyRequest creates or updates a Bifrost virtual key.
type VirtualKeyRequest struct {
	Name            string                     `json:"name"`
	Description     string                     `json:"description,omitempty"`
	IsActive        *bool                      `json:"is_active,omitempty"`
	Budgets         []BudgetSpec               `json:"budgets,omitempty"`
	ProviderConfigs []VirtualKeyProviderConfig `json:"provider_configs,omitempty"`
	Fallbacks       []FallbackTarget           `json:"fallbacks,omitempty"`
}

// ListVirtualKeys queries GET /api/governance/virtual-keys and masks raw secrets for UI display.
func (c *Client) ListVirtualKeys(ctx context.Context) ([]VirtualKey, error) {
	body, err := c.doJSON(ctx, http.MethodGet, "/api/governance/virtual-keys", nil, true)
	if err != nil {
		return nil, err
	}
	var direct []VirtualKey
	if err := json.Unmarshal(body, &direct); err == nil {
		return sanitizeVirtualKeys(direct), nil
	}
	var wrapped struct {
		VirtualKeys []VirtualKey `json:"virtual_keys"`
		Data        []VirtualKey `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return nil, fmt.Errorf("decode bifrost virtual keys: %w", err)
	}
	if len(wrapped.VirtualKeys) > 0 {
		return sanitizeVirtualKeys(wrapped.VirtualKeys), nil
	}
	return sanitizeVirtualKeys(wrapped.Data), nil
}

// CreateVirtualKey creates a new virtual key via POST /api/governance/virtual-keys.
func (c *Client) CreateVirtualKey(ctx context.Context, req VirtualKeyRequest) (VirtualKey, error) {
	body, err := c.doJSON(ctx, http.MethodPost, "/api/governance/virtual-keys", req, true)
	if err != nil {
		return VirtualKey{}, err
	}
	return decodeSingleVirtualKey(body)
}

// UpdateVirtualKey updates an existing virtual key via PUT /api/governance/virtual-keys/{id}.
func (c *Client) UpdateVirtualKey(ctx context.Context, id string, req VirtualKeyRequest) (VirtualKey, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return VirtualKey{}, errors.New("virtual key id is required")
	}
	body, err := c.doJSON(ctx, http.MethodPut, "/api/governance/virtual-keys/"+url.PathEscape(id), req, true)
	if err != nil {
		return VirtualKey{}, err
	}
	return decodeSingleVirtualKey(body)
}

// LogsQuery scopes /api/logs and /api/logs/stats requests.
type LogsQuery struct {
	Limit     int
	Offset    int
	Provider  string
	Model     string
	StartTime string
	EndTime   string
}

func (q LogsQuery) encode() string {
	vals := url.Values{}
	if q.Limit > 0 {
		vals.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Offset > 0 {
		vals.Set("offset", strconv.Itoa(q.Offset))
	}
	if strings.TrimSpace(q.Provider) != "" {
		vals.Set("providers", strings.TrimSpace(q.Provider))
	}
	if strings.TrimSpace(q.Model) != "" {
		vals.Set("models", strings.TrimSpace(q.Model))
	}
	if strings.TrimSpace(q.StartTime) != "" {
		vals.Set("start_time", strings.TrimSpace(q.StartTime))
	}
	if strings.TrimSpace(q.EndTime) != "" {
		vals.Set("end_time", strings.TrimSpace(q.EndTime))
	}
	encoded := vals.Encode()
	if encoded == "" {
		return ""
	}
	return "?" + encoded
}

// UsageStats summarizes metered Bifrost usage from GET /api/logs/stats.
type UsageStats struct {
	TotalRequests int64   `json:"totalRequests"`
	SuccessRate   float64 `json:"successRate"`
	AverageLatency float64 `json:"averageLatencyMs"`
	TotalTokens   int64   `json:"totalTokens"`
	TotalCostUSD  float64 `json:"totalCostUsd"`
	FallbackCount int64   `json:"fallbackCount"`
	ErrorCount    int64   `json:"errorCount"`
}

// LogEntry is a compact gateway request trace from GET /api/logs.
type LogEntry struct {
	ID           string  `json:"id"`
	Timestamp    string  `json:"timestamp,omitempty"`
	Provider     string  `json:"provider,omitempty"`
	Model        string  `json:"model,omitempty"`
	Status       string  `json:"status,omitempty"`
	LatencyMs    float64 `json:"latencyMs,omitempty"`
	TotalTokens  int64   `json:"totalTokens,omitempty"`
	CostUSD      float64 `json:"costUsd,omitempty"`
	FallbackUsed bool    `json:"fallbackUsed,omitempty"`
}

// LogsStats fetches aggregated stats from GET /api/logs/stats.
func (c *Client) LogsStats(ctx context.Context, query LogsQuery) (UsageStats, error) {
	body, err := c.doJSON(ctx, http.MethodGet, "/api/logs/stats"+query.encode(), nil, true)
	if err != nil {
		return UsageStats{}, err
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return UsageStats{}, fmt.Errorf("decode bifrost /api/logs/stats: %w", err)
	}
	// Handle optional {"stats": {...}} or {"data": {...}} envelopes.
	if nested, ok := raw["stats"].(map[string]any); ok {
		raw = nested
	} else if nested, ok := raw["data"].(map[string]any); ok {
		raw = nested
	}
	return UsageStats{
		TotalRequests:  int64Field(raw, "total_requests", "totalRequests", "requests"),
		SuccessRate:    floatField(raw, "success_rate", "successRate"),
		AverageLatency: floatField(raw, "average_latency", "averageLatency", "avg_latency_ms", "averageLatencyMs"),
		TotalTokens:    int64Field(raw, "total_tokens", "totalTokens"),
		TotalCostUSD:   floatField(raw, "total_cost", "totalCost", "total_cost_usd", "totalCostUsd"),
		FallbackCount:  int64Field(raw, "fallback_count", "fallbackCount", "fallbacks"),
		ErrorCount:     int64Field(raw, "error_count", "errorCount", "failed_requests"),
	}, nil
}

// RecentLogs fetches recent gateway logs from GET /api/logs.
func (c *Client) RecentLogs(ctx context.Context, query LogsQuery) ([]LogEntry, error) {
	if query.Limit <= 0 {
		query.Limit = 20
	}
	body, err := c.doJSON(ctx, http.MethodGet, "/api/logs"+query.encode(), nil, true)
	if err != nil {
		return nil, err
	}
	var rawItems []map[string]any
	if err := json.Unmarshal(body, &rawItems); err != nil {
		var wrapped struct {
			Logs []map[string]any `json:"logs"`
			Data []map[string]any `json:"data"`
		}
		if err2 := json.Unmarshal(body, &wrapped); err2 != nil {
			return nil, fmt.Errorf("decode bifrost /api/logs: %w", err2)
		}
		rawItems = wrapped.Logs
		if len(rawItems) == 0 {
			rawItems = wrapped.Data
		}
	}
	out := make([]LogEntry, 0, len(rawItems))
	for _, item := range rawItems {
		entry := LogEntry{
			ID:           stringField(item, "id", "request_id"),
			Timestamp:    stringField(item, "timestamp", "created_at"),
			Provider:     stringField(item, "provider"),
			Model:        stringField(item, "model"),
			Status:       stringField(item, "status"),
			LatencyMs:    floatField(item, "latency", "latency_ms", "latencyMs"),
			TotalTokens:  int64Field(item, "total_tokens", "totalTokens"),
			CostUSD:      floatField(item, "cost", "cost_usd", "costUsd"),
			FallbackUsed: boolField(item, "fallback_used", "fallbackUsed"),
		}
		if usage, ok := item["token_usage"].(map[string]any); ok && entry.TotalTokens == 0 {
			entry.TotalTokens = int64Field(usage, "total_tokens", "totalTokens")
		}
		out = append(out, entry)
	}
	return out, nil
}

// GetConfig reads GET /api/config.
func (c *Client) GetConfig(ctx context.Context) (map[string]any, error) {
	body, err := c.doJSON(ctx, http.MethodGet, "/api/config", nil, true)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode bifrost /api/config: %w", err)
	}
	return out, nil
}

// PutConfig updates PUT /api/config.
func (c *Client) PutConfig(ctx context.Context, cfg map[string]any) error {
	_, err := c.doJSON(ctx, http.MethodPut, "/api/config", cfg, true)
	return err
}

// HTTPStatusError captures a non-2xx response from Bifrost.
type HTTPStatusError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *HTTPStatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("bifrost %s %s returned HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
	}
	return fmt.Sprintf("bifrost %s %s returned HTTP %d", e.Method, e.Path, e.StatusCode)
}

func (c *Client) doJSON(ctx context.Context, method, path string, payload any, management bool) ([]byte, error) {
	u, err := c.endpoint(path)
	if err != nil {
		return nil, err
	}
	var bodyReader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if management {
		if c.AdminToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.AdminToken)
		}
	} else if c.VirtualKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.VirtualKey)
		if strings.HasPrefix(c.VirtualKey, "sk-bf-") {
			req.Header.Set("x-bf-vk", c.VirtualKey)
		}
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, defaultHTTPMaxLen))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 512 {
			msg = msg[:512]
		}
		return nil, &HTTPStatusError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: msg}
	}
	return raw, nil
}

// MaskSecret returns a safe display preview without exposing the full credential.
func MaskSecret(secret string) string {
	s := strings.TrimSpace(secret)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "env.") {
		return s
	}
	if len(s) <= 8 {
		return "****"
	}
	return s[:6] + "…" + s[len(s)-4:]
}

func decodeProvidersPayload(body []byte) ([]ProviderSummary, error) {
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err == nil {
		return normalizeProviderSlice(list), nil
	}
	var wrapped struct {
		Providers json.RawMessage `json:"providers"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil {
		raw := wrapped.Providers
		if len(raw) == 0 {
			raw = wrapped.Data
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &list); err == nil {
				return normalizeProviderSlice(list), nil
			}
			var byName map[string]map[string]any
			if err := json.Unmarshal(raw, &byName); err == nil {
				return normalizeProviderMap(byName), nil
			}
		}
	}
	var byName map[string]map[string]any
	if err := json.Unmarshal(body, &byName); err == nil {
		return normalizeProviderMap(byName), nil
	}
	return nil, errors.New("unexpected bifrost /api/providers payload")
}

func normalizeProviderSlice(items []map[string]any) []ProviderSummary {
	out := make([]ProviderSummary, 0, len(items))
	for _, item := range items {
		name := stringField(item, "name", "provider")
		if name == "" {
			continue
		}
		out = append(out, normalizeProviderEntry(name, item))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func normalizeProviderMap(items map[string]map[string]any) []ProviderSummary {
	out := make([]ProviderSummary, 0, len(items))
	for name, item := range items {
		if strings.TrimSpace(name) == "" {
			continue
		}
		out = append(out, normalizeProviderEntry(name, item))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func normalizeProviderEntry(name string, item map[string]any) ProviderSummary {
	summary := ProviderSummary{
		Name:   strings.TrimSpace(name),
		Status: stringField(item, "status"),
	}
	if nc, ok := item["network_config"].(map[string]any); ok {
		summary.NetworkConfig = nc
		summary.BaseURL = stringField(nc, "base_url", "baseUrl")
	}
	modelSet := map[string]bool{}
	if keysRaw, ok := item["keys"].([]any); ok {
		for _, k := range keysRaw {
			km, ok := k.(map[string]any)
			if !ok {
				continue
			}
			val := stringField(km, "value")
			models := stringSliceField(km, "models")
			for _, m := range models {
				modelSet[m] = true
			}
			summary.Keys = append(summary.Keys, ProviderKeySummary{
				Name:       stringField(km, "name", "id"),
				Models:     models,
				Weight:     floatField(km, "weight"),
				Configured: val != "",
				Preview:    MaskSecret(val),
			})
		}
	}
	summary.KeyCount = len(summary.Keys)
	if summary.KeyCount == 0 {
		if kc := int(int64Field(item, "key_count", "keyCount")); kc > 0 {
			summary.KeyCount = kc
		}
	}
	for _, m := range stringSliceField(item, "models") {
		modelSet[m] = true
	}
	for m := range modelSet {
		summary.Models = append(summary.Models, m)
	}
	sort.Strings(summary.Models)
	if summary.Status == "" {
		if summary.KeyCount > 0 {
			summary.Status = "configured"
		} else {
			summary.Status = "unconfigured"
		}
	}
	return summary
}

func sanitizeVirtualKeys(items []VirtualKey) []VirtualKey {
	out := make([]VirtualKey, len(items))
	for i, vk := range items {
		vk.ValuePreview = MaskSecret(vk.Value)
		vk.Value = ""
		out[i] = vk
	}
	return out
}

func decodeSingleVirtualKey(body []byte) (VirtualKey, error) {
	var wrapped struct {
		VirtualKey *VirtualKey `json:"virtual_key"`
		Data       *VirtualKey `json:"data"`
	}
	if json.Unmarshal(body, &wrapped) == nil {
		if wrapped.VirtualKey != nil && (wrapped.VirtualKey.ID != "" || wrapped.VirtualKey.Name != "") {
			vk := *wrapped.VirtualKey
			vk.ValuePreview = MaskSecret(vk.Value)
			return vk, nil
		}
		if wrapped.Data != nil && (wrapped.Data.ID != "" || wrapped.Data.Name != "") {
			vk := *wrapped.Data
			vk.ValuePreview = MaskSecret(vk.Value)
			return vk, nil
		}
	}
	var vk VirtualKey
	if err := json.Unmarshal(body, &vk); err != nil {
		return VirtualKey{}, fmt.Errorf("decode bifrost virtual key: %w", err)
	}
	vk.ValuePreview = MaskSecret(vk.Value)
	return vk, nil
}

func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func stringSliceField(m map[string]any, key string) []string {
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func int64Field(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case int:
			return int64(v)
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}

func floatField(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return v
		case int64:
			return float64(v)
		case int:
			return float64(v)
		case string:
			if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				return n
			}
		}
	}
	return 0
}

func boolField(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k].(bool); ok {
			return v
		}
	}
	return false
}
