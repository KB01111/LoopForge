package bifrost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientEndpoints(t *testing.T) {
	var gotAdminAuth, gotVKAuth, gotBFVK string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": "1.2.3"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			gotVKAuth = r.Header.Get("Authorization")
			gotBFVK = r.Header.Get("x-bf-vk")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "anthropic/claude-sonnet-4-6", "owned_by": "anthropic"},
					{"id": "openai/gpt-5.4", "owned_by": "openai"},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/providers":
			gotAdminAuth = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"openai": map[string]any{
					"keys": []map[string]any{
						{"name": "primary", "value": "sk-secret-value-1234567890", "models": []string{"gpt-5.4"}, "weight": 1.0},
					},
					"network_config": map[string]any{"base_url": "https://api.openai.com/v1"},
				},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/providers/anthropic":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/governance/virtual-keys":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"virtual_keys": []map[string]any{
					{"id": "vk-1", "name": "loopforge-default", "value": "sk-bf-1234567890abcdef", "is_active": true},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/governance/virtual-keys":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"virtual_key": map[string]any{"id": "vk-2", "name": "ci", "value": "sk-bf-abcdef1234567890", "is_active": true},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/governance/virtual-keys/vk-2":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "vk-2", "name": "ci-updated", "is_active": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/logs/stats":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_requests":  42,
				"success_rate":    97.6,
				"average_latency": 310.5,
				"total_tokens":    125000,
				"total_cost":      1.42,
				"fallback_count":  3,
				"error_count":     1,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/logs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"logs": []map[string]any{
					{"id": "log-1", "provider": "anthropic", "model": "claude-sonnet-4-6", "status": "success", "latency": 220, "total_tokens": 900, "cost": 0.012},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"client": map[string]any{"enforce_auth_on_inference": true}})
		case r.Method == http.MethodPut && r.URL.Path == "/api/config":
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "bfak_admin", "sk-bf-vk123456")
	ctx := context.Background()

	health, err := client.Health(ctx)
	if err != nil || !health.OK || health.Version != "1.2.3" {
		t.Fatalf("Health = %+v, err=%v", health, err)
	}

	models, err := client.ListModels(ctx)
	if err != nil || len(models) != 2 || models[0].Provider != "anthropic" {
		t.Fatalf("ListModels = %+v, err=%v", models, err)
	}
	if gotVKAuth != "Bearer sk-bf-vk123456" || gotBFVK != "sk-bf-vk123456" {
		t.Fatalf("expected VK headers, got auth=%q x-bf-vk=%q", gotVKAuth, gotBFVK)
	}

	providers, err := client.ListProviders(ctx)
	if err != nil || len(providers) != 1 || providers[0].Name != "openai" || providers[0].KeyCount != 1 {
		t.Fatalf("ListProviders = %+v, err=%v", providers, err)
	}
	if gotAdminAuth != "Bearer bfak_admin" {
		t.Fatalf("expected admin auth header, got %q", gotAdminAuth)
	}
	if strings.Contains(providers[0].Keys[0].Preview, "secret-value") {
		t.Fatalf("provider key preview leaked secret: %q", providers[0].Keys[0].Preview)
	}

	if err := client.UpsertProvider(ctx, "anthropic", ProviderUpsertRequest{
		Keys: []ProviderKeyInput{{Name: "main", Value: "env.ANTHROPIC_API_KEY", Weight: 1}},
	}); err != nil {
		t.Fatalf("UpsertProvider err=%v", err)
	}

	vks, err := client.ListVirtualKeys(ctx)
	if err != nil || len(vks) != 1 || vks[0].Value != "" || vks[0].ValuePreview == "" {
		t.Fatalf("ListVirtualKeys = %+v, err=%v", vks, err)
	}

	active := true
	created, err := client.CreateVirtualKey(ctx, VirtualKeyRequest{Name: "ci", IsActive: &active})
	if err != nil || created.ID != "vk-2" {
		t.Fatalf("CreateVirtualKey = %+v, err=%v", created, err)
	}
	updated, err := client.UpdateVirtualKey(ctx, "vk-2", VirtualKeyRequest{Name: "ci-updated"})
	if err != nil || updated.Name != "ci-updated" {
		t.Fatalf("UpdateVirtualKey = %+v, err=%v", updated, err)
	}

	stats, err := client.LogsStats(ctx, LogsQuery{})
	if err != nil || stats.TotalRequests != 42 || stats.TotalTokens != 125000 || stats.FallbackCount != 3 {
		t.Fatalf("LogsStats = %+v, err=%v", stats, err)
	}
	logs, err := client.RecentLogs(ctx, LogsQuery{Limit: 10})
	if err != nil || len(logs) != 1 || logs[0].ID != "log-1" {
		t.Fatalf("RecentLogs = %+v, err=%v", logs, err)
	}

	cfg, err := client.GetConfig(ctx)
	if err != nil || cfg["client"] == nil {
		t.Fatalf("GetConfig = %+v, err=%v", cfg, err)
	}
	if err := client.PutConfig(ctx, cfg); err != nil {
		t.Fatalf("PutConfig err=%v", err)
	}
}

func TestResolveHarnessEndpointAllHarnesses(t *testing.T) {
	base := "http://127.0.0.1:8091/"
	cases := []struct {
		kind     string
		protocol string
		wantURL  string
		wantProt string
		wantOK   bool
	}{
		{"claude-code", "", "http://127.0.0.1:8091/anthropic", ProtocolMessages, true},
		{"claude", "", "http://127.0.0.1:8091/anthropic", ProtocolMessages, true},
		{"codex", "", "http://127.0.0.1:8091/v1", ProtocolResponses, true},
		{"opencode", "chat", "http://127.0.0.1:8091/v1", ProtocolChat, true},
		{"opencode", "messages", "http://127.0.0.1:8091/anthropic", ProtocolMessages, true},
		{"agy", "", "http://127.0.0.1:8091/genai", ProtocolGemini, true},
		{"agy", "chat", "http://127.0.0.1:8091/v1", ProtocolChat, true},
		{"kimi", "responses", "http://127.0.0.1:8091/v1", ProtocolResponses, true},
		{"pi", "chat", "http://127.0.0.1:8091/v1", ProtocolChat, true},
		{"dsh", "messages", "http://127.0.0.1:8091/anthropic", ProtocolMessages, true},
		{"zcode", "", "http://127.0.0.1:8091/anthropic", ProtocolMessages, true},
		{"prime-agent", "", "http://127.0.0.1:8091/v1", ProtocolChat, true},
		{"prime-agent", "responses", "http://127.0.0.1:8091/v1", ProtocolResponses, true},
		{"prime-agent", "messages", "http://127.0.0.1:8091/anthropic", ProtocolMessages, true},
		{"gemini-cli", "", "http://127.0.0.1:8091/genai", ProtocolGemini, true},
		{"goose", "", "http://127.0.0.1:8091/v1", ProtocolChat, true},
		{"goose", "messages", "http://127.0.0.1:8091/anthropic", ProtocolMessages, true},
		{"codebuddy", "", "", "", false},
		{"cursor", "", "", "", false},
	}
	for _, tc := range cases {
		gotURL, gotProt, gotOK := ResolveHarnessEndpoint(tc.kind, tc.protocol, base)
		if gotURL != tc.wantURL || gotProt != tc.wantProt || gotOK != tc.wantOK {
			t.Errorf("ResolveHarnessEndpoint(%q, %q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.kind, tc.protocol, gotURL, gotProt, gotOK, tc.wantURL, tc.wantProt, tc.wantOK)
		}
	}
}

func TestApplyRunCredentialsPolicies(t *testing.T) {
	policy := RoutingPolicy{
		Enabled:    true,
		Mode:       ModeFallbackWhenEmpty,
		BaseURL:    "http://127.0.0.1:8091",
		SandboxURL: "http://host.docker.internal:8091",
		VirtualKey: "sk-bf-platform",
	}

	// fallback-when-empty keeps user's existing Claude key, fills empty Codex & Prime Agent.
	bundle := RunCredentialBundle{
		ClaudeCode: HarnessCredential{APIKey: "sk-ant-user", BaseURL: "https://api.anthropic.com"},
		AgentCLI: map[string]HarnessCredential{
			"prime-agent": {Protocol: ProtocolResponses},
		},
	}
	ApplyRunCredentials(policy, true, &bundle)
	if bundle.ClaudeCode.APIKey != "sk-ant-user" {
		t.Fatalf("fallback-when-empty overwrote user Claude key: %+v", bundle.ClaudeCode)
	}
	if bundle.Codex.APIKey != "sk-bf-platform" || bundle.Codex.BaseURL != "http://host.docker.internal:8091/v1" {
		t.Fatalf("Codex not routed to sandbox Bifrost: %+v", bundle.Codex)
	}
	if pa := bundle.AgentCLI["prime-agent"]; pa.APIKey != "sk-bf-platform" || pa.BaseURL != "http://host.docker.internal:8091/v1" || pa.Protocol != ProtocolResponses {
		t.Fatalf("prime-agent not routed: %+v", pa)
	}
	if gc := bundle.AgentCLI["gemini-cli"]; gc.APIKey != "sk-bf-platform" || gc.BaseURL != "http://host.docker.internal:8091/genai" {
		t.Fatalf("gemini-cli not routed: %+v", gc)
	}
	if gs := bundle.AgentCLI["goose"]; gs.APIKey != "sk-bf-platform" || gs.BaseURL != "http://host.docker.internal:8091/v1" {
		t.Fatalf("goose not routed: %+v", gs)
	}

	// force-all overrides user's existing key and routes to host URL when useSandboxURL=false.
	policy.Mode = ModeForceAll
	ApplyRunCredentials(policy, false, &bundle)
	if bundle.ClaudeCode.APIKey != "sk-bf-platform" || bundle.ClaudeCode.BaseURL != "http://127.0.0.1:8091/anthropic" {
		t.Fatalf("force-all did not override ClaudeCode: %+v", bundle.ClaudeCode)
	}

	// off leaves credentials untouched.
	policy.Mode = ModeOff
	offBundle := RunCredentialBundle{}
	ApplyRunCredentials(policy, false, &offBundle)
	if offBundle.Codex.APIKey != "" || len(offBundle.AgentCLI) != 0 {
		t.Fatalf("off mode mutated credentials: %+v", offBundle)
	}
}

func TestEvalClientEvaluateAndProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("x-bf-vk") != "sk-bf-eval" {
			http.Error(w, "missing vk", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "openai/gpt-5.4",
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": `{"score":92,"passed":true,"summary":"All holdout checks pass"}`}},
			},
			"usage": map[string]any{"prompt_tokens": 120, "completion_tokens": 35, "total_tokens": 155},
		})
	}))
	defer srv.Close()

	ec := NewEvalClient(srv.URL, "sk-bf-eval")
	resp, err := ec.Evaluate(context.Background(), "openai/gpt-5.4", "grade strictly", "run facts")
	if err != nil {
		t.Fatalf("Evaluate err=%v", err)
	}
	if resp.TotalTokens != 155 || !strings.Contains(resp.Content, `"score":92`) {
		t.Fatalf("unexpected EvalResponse: %+v", resp)
	}
	probe, err := ec.TestCompletion(context.Background(), "openai/gpt-5.4")
	if err != nil || !probe.OK || probe.Model != "openai/gpt-5.4" {
		t.Fatalf("unexpected TestProbeResult: %+v, err=%v", probe, err)
	}
}
