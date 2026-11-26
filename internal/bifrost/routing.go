package bifrost

import "strings"

const (
	ModeOff               = "off"
	ModeFallbackWhenEmpty = "fallback-when-empty"
	ModeForceAll          = "force-all"
)

const (
	ProtocolChat      = "chat"
	ProtocolResponses = "responses"
	ProtocolMessages  = "messages"
	ProtocolGemini    = "gemini"
)

// NormalizeMode maps raw routing mode strings onto a canonical mode.
func NormalizeMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ModeForceAll:
		return ModeForceAll
	case ModeFallbackWhenEmpty:
		return ModeFallbackWhenEmpty
	case ModeOff:
		return ModeOff
	default:
		return ""
	}
}

// ValidMode reports whether raw is an accepted routing mode.
func ValidMode(raw string) bool {
	return NormalizeMode(raw) != ""
}

// RoutingPolicy controls whether and how harness LLM traffic routes through Bifrost.
type RoutingPolicy struct {
	Enabled    bool
	Mode       string
	BaseURL    string
	SandboxURL string
	VirtualKey string
}

// Active reports whether this policy should inject Bifrost credentials.
func (p RoutingPolicy) Active() bool {
	if !p.Enabled {
		return false
	}
	mode := NormalizeMode(p.Mode)
	if mode == "" || mode == ModeOff {
		return false
	}
	return strings.TrimSpace(p.BaseURL) != "" && strings.TrimSpace(p.VirtualKey) != ""
}

// EffectiveBaseURL returns SandboxURL for container sandboxes when configured,
// otherwise BaseURL.
func (p RoutingPolicy) EffectiveBaseURL(useSandboxURL bool) string {
	if useSandboxURL && strings.TrimSpace(p.SandboxURL) != "" {
		return strings.TrimRight(strings.TrimSpace(p.SandboxURL), "/")
	}
	return strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
}

// SupportedAgentCLIKinds lists every agent CLI kind that supports Bifrost base URL routing.
var SupportedAgentCLIKinds = []string{
	"opencode",
	"agy",
	"kimi",
	"pi",
	"dsh",
	"zcode",
	"prime-agent",
	"gemini-cli",
	"goose",
}

// ResolveHarnessEndpoint maps a coding harness kind and wire protocol to the
// exact Bifrost protocol-compatible base URL path.
//
// Canonical Bifrost endpoint paths:
//   - OpenAI Chat / Responses: <base>/v1
//   - Anthropic Messages:      <base>/anthropic
//   - Google GenAI:            <base>/genai
func ResolveHarnessEndpoint(kind, protocol, baseURL string) (endpoint string, effectiveProtocol string, supported bool) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return "", "", false
	}
	k := strings.ToLower(strings.TrimSpace(kind))
	p := strings.ToLower(strings.TrimSpace(protocol))

	switch k {
	case "claude", "claude-code":
		return base + "/anthropic", ProtocolMessages, true
	case "codex":
		return base + "/v1", ProtocolResponses, true
	case "gemini-cli":
		return base + "/genai", ProtocolGemini, true
	case "agy":
		if p == ProtocolChat {
			return base + "/v1", ProtocolChat, true
		}
		return base + "/genai", ProtocolGemini, true
	case "prime-agent":
		switch p {
		case ProtocolMessages:
			return base + "/anthropic", ProtocolMessages, true
		case ProtocolResponses:
			return base + "/v1", ProtocolResponses, true
		default:
			return base + "/v1", ProtocolChat, true
		}
	case "goose":
		if p == ProtocolMessages {
			return base + "/anthropic", ProtocolMessages, true
		}
		return base + "/v1", ProtocolChat, true
	case "opencode", "kimi", "pi", "dsh":
		switch p {
		case ProtocolMessages:
			return base + "/anthropic", ProtocolMessages, true
		case ProtocolResponses:
			return base + "/v1", ProtocolResponses, true
		default:
			return base + "/v1", ProtocolChat, true
		}
	case "zcode":
		switch p {
		case ProtocolChat:
			return base + "/v1", ProtocolChat, true
		case ProtocolResponses:
			return base + "/v1", ProtocolResponses, true
		default:
			return base + "/anthropic", ProtocolMessages, true
		}
	case "codebuddy", "cursor":
		// Proprietary vendor-only endpoints without custom Base URL support.
		return "", "", false
	default:
		return "", "", false
	}
}

// HarnessCredential is the minimal credential tuple needed for Bifrost routing
// without coupling package bifrost to OS-specific runtime packages.
type HarnessCredential struct {
	APIKey   string
	BaseURL  string
	Protocol string
}

// RunCredentialBundle is the mutable credential set mutated by ApplyRunCredentials.
type RunCredentialBundle struct {
	Codex      HarnessCredential
	ClaudeCode HarnessCredential
	AgentCLI   map[string]HarnessCredential
}

// ResolveCredential determines the effective API key, base URL, and protocol for
// one harness invocation under policy.
func ResolveCredential(policy RoutingPolicy, useSandboxURL bool, kind, protocol, existingKey, existingBaseURL string) (apiKey, baseURL, effectiveProtocol string, routed bool) {
	apiKey = strings.TrimSpace(existingKey)
	baseURL = strings.TrimSpace(existingBaseURL)
	effectiveProtocol = strings.TrimSpace(protocol)

	if !policy.Active() {
		return apiKey, baseURL, effectiveProtocol, false
	}
	targetURL, resolvedProto, supported := ResolveHarnessEndpoint(kind, effectiveProtocol, policy.EffectiveBaseURL(useSandboxURL))
	if !supported {
		return apiKey, baseURL, effectiveProtocol, false
	}
	mode := NormalizeMode(policy.Mode)
	switch mode {
	case ModeForceAll:
		return strings.TrimSpace(policy.VirtualKey), targetURL, resolvedProto, true
	case ModeFallbackWhenEmpty:
		if apiKey == "" {
			return strings.TrimSpace(policy.VirtualKey), targetURL, resolvedProto, true
		}
		return apiKey, baseURL, effectiveProtocol, false
	default:
		return apiKey, baseURL, effectiveProtocol, false
	}
}

// ApplyRunCredentials injects Bifrost endpoints and virtual keys into creds
// according to policy (off, fallback-when-empty, force-all).
func ApplyRunCredentials(policy RoutingPolicy, useSandboxURL bool, creds *RunCredentialBundle) {
	if creds == nil || !policy.Active() {
		return
	}
	if key, base, proto, ok := ResolveCredential(policy, useSandboxURL, "codex", creds.Codex.Protocol, creds.Codex.APIKey, creds.Codex.BaseURL); ok {
		creds.Codex.APIKey = key
		creds.Codex.BaseURL = base
		creds.Codex.Protocol = proto
	}
	if key, base, proto, ok := ResolveCredential(policy, useSandboxURL, "claude-code", creds.ClaudeCode.Protocol, creds.ClaudeCode.APIKey, creds.ClaudeCode.BaseURL); ok {
		creds.ClaudeCode.APIKey = key
		creds.ClaudeCode.BaseURL = base
		creds.ClaudeCode.Protocol = proto
	}
	if creds.AgentCLI == nil {
		creds.AgentCLI = make(map[string]HarnessCredential, len(SupportedAgentCLIKinds))
	}
	for _, kind := range SupportedAgentCLIKinds {
		existing := creds.AgentCLI[kind]
		if key, base, proto, ok := ResolveCredential(policy, useSandboxURL, kind, existing.Protocol, existing.APIKey, existing.BaseURL); ok {
			existing.APIKey = key
			existing.BaseURL = base
			existing.Protocol = proto
			creds.AgentCLI[kind] = existing
		}
	}
}
