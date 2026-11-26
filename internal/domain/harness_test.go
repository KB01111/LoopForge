package domain

import (
	"slices"
	"testing"
)

func TestBuiltInHarnessesCoversAll13HarnessesAndCapabilities(t *testing.T) {
	harnesses := BuiltInHarnesses()
	if len(harnesses) != 13 {
		t.Fatalf("BuiltInHarnesses() count = %d, want 13", len(harnesses))
	}
	seenIDs := map[string]bool{}
	seenKinds := map[string]bool{}
	for _, h := range harnesses {
		if h.ID == "" || h.Kind == "" || h.Name == "" || h.Binary == "" {
			t.Fatalf("incomplete descriptor: %+v", h)
		}
		if seenIDs[h.ID] || seenKinds[h.Kind] {
			t.Fatalf("duplicate descriptor id=%q kind=%q", h.ID, h.Kind)
		}
		seenIDs[h.ID] = true
		seenKinds[h.Kind] = true
		if len(h.Efforts) == 0 {
			t.Errorf("harness %q has no efforts", h.Kind)
		}
		if !slices.Equal(h.Efforts, ExecutorEffortLevels[h.Kind]) {
			t.Errorf("harness %q efforts %v != ExecutorEffortLevels %v", h.Kind, h.Efforts, ExecutorEffortLevels[h.Kind])
		}
		if IsAgentCLIKind(h.Kind) {
			if want := AgentCLIProtocolOptions[h.Kind]; !slices.Equal(h.Protocols, want) {
				t.Errorf("agent CLI %q protocols %v != AgentCLIProtocolOptions %v", h.Kind, h.Protocols, want)
			}
		}
	}
	for _, kind := range AgentCLIExecutorKinds {
		if !seenKinds[kind] {
			t.Errorf("AgentCLIExecutorKinds entry %q missing from BuiltInHarnesses()", kind)
		}
	}

	prime, ok := HarnessByKind("prime-agent")
	if !ok {
		t.Fatal("prime-agent missing from HarnessByKind")
	}
	if !prime.Capabilities.RLMLongHorizon || !prime.Capabilities.PersistentREPL || !prime.Capabilities.SelfRefine || !prime.Capabilities.MCP {
		t.Fatalf("prime-agent missing expected RLM capabilities: %+v", prime.Capabilities)
	}
	if got := prime.Bifrost.EndpointFor("", "chat"); got != "http://127.0.0.1:8091/v1" {
		t.Fatalf("prime-agent chat Bifrost endpoint = %q, want http://127.0.0.1:8091/v1", got)
	}
	if got := prime.Bifrost.EndpointFor("http://bifrost:8091/", "messages"); got != "http://bifrost:8091/anthropic" {
		t.Fatalf("prime-agent messages Bifrost endpoint = %q, want http://bifrost:8091/anthropic", got)
	}

	gemini, ok := HarnessByKind("gemini-cli")
	if !ok {
		t.Fatal("gemini-cli missing")
	}
	if gemini.Binary != "gemini" || !gemini.Capabilities.NativeApprovalHook {
		t.Fatalf("unexpected gemini-cli descriptor: %+v", gemini)
	}
	if got := gemini.Bifrost.EndpointFor("", ""); got != "http://127.0.0.1:8091/genai" {
		t.Fatalf("gemini-cli Bifrost endpoint = %q, want http://127.0.0.1:8091/genai", got)
	}

	goose, ok := HarnessByKind("goose")
	if !ok {
		t.Fatal("goose missing")
	}
	if goose.Binary != "goose" || goose.Category != "mcp-native" || !goose.Capabilities.MCP {
		t.Fatalf("unexpected goose descriptor: %+v", goose)
	}
	if got := goose.Bifrost.EndpointFor("", "chat"); got != "http://127.0.0.1:8091/v1" {
		t.Fatalf("goose Bifrost endpoint = %q, want http://127.0.0.1:8091/v1", got)
	}

	// Alias resolution for claude / claude-code
	claudeByID, ok1 := HarnessByKind("claude")
	claudeByKind, ok2 := HarnessByKind("claude-code")
	if !ok1 || !ok2 || claudeByID.Kind != "claude-code" || claudeByKind.ID != "claude" {
		t.Fatalf("claude alias lookup failed: %+v / %+v", claudeByID, claudeByKind)
	}
}

func TestHarnessOverrideValidation(t *testing.T) {
	if !(&HarnessOverride{}).IsZero() {
		t.Fatal("empty override must be zero")
	}
	valid := &HarnessOverride{ExecutorID: "prime-agent", Model: "claude-sonnet-4-6", Effort: "high", RLMMaxDepth: 4, RLMMaxSteps: 50, RLMTokenBudget: 100000, AutoRefine: true}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid override failed: %v", err)
	}
	if err := (&HarnessOverride{ExecutorID: "unknown-cli"}).Validate(); err == nil {
		t.Fatal("unknown executor accepted")
	}
	if err := (&HarnessOverride{ExecutorID: "gemini-cli", Effort: "ultracode"}).Validate(); err == nil {
		t.Fatal("unsupported effort accepted")
	}
	if err := (&HarnessOverride{ExecutorID: "prime-agent", RLMMaxDepth: 99}).Validate(); err == nil {
		t.Fatal("out-of-range RLMMaxDepth accepted")
	}
	if err := (&HarnessOverride{ExecutorID: "prime-agent", RLMTokenBudget: -1}).Validate(); err == nil {
		t.Fatal("negative RLMTokenBudget accepted")
	}
}
