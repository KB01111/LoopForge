package domain

import (
	"fmt"
	"strings"
)

// DefaultBifrostPort and DefaultBifrostURL identify the canonical local Bifrost
// LLM router port shared across LoopForge backend, harnesses, and Tauri desktop.
const (
	DefaultBifrostPort = 8091
	DefaultBifrostURL  = "http://127.0.0.1:8091"
)

// HarnessCapabilities declares the runtime, autonomy, and governance features a
// coding harness supports so LoopForge UI, workflows, and Tauri desktop can
// match tasks to harnesses without hardcoding per-CLI heuristics.
type HarnessCapabilities struct {
	RLMLongHorizon     bool     `json:"rlmLongHorizon,omitempty"`
	PersistentREPL     bool     `json:"persistentRepl,omitempty"`
	SelfRefine         bool     `json:"selfRefine,omitempty"`
	MCP                bool     `json:"mcp"`
	NativeHooks        bool     `json:"nativeHooks,omitempty"`
	NativeApprovalHook bool     `json:"nativeApprovalHook,omitempty"`
	StreamJSON         bool     `json:"streamJson"`
	SessionResume      bool     `json:"sessionResume"`
	ReadOnlyChat       bool     `json:"readOnlyChat"`
	LocalLogin         bool     `json:"localLogin,omitempty"`
	SandboxDrivers     []string `json:"sandboxDrivers"`
}

// HarnessBifrostRouting describes how a harness routes inference through the
// Bifrost LLM gateway (default http://127.0.0.1:8091).
type HarnessBifrostRouting struct {
	Supported       bool              `json:"supported"`
	DefaultProtocol string            `json:"defaultProtocol,omitempty"` // chat, responses, messages, gemini
	ProtocolPaths   map[string]string `json:"protocolPaths,omitempty"`   // protocol -> path suffix ("/v1", "/anthropic", "/genai")
	ConfigMechanism string            `json:"configMechanism,omitempty"` // env, models_json, toml, cli_flag, config_yaml, patch
}

// EndpointFor resolves the full Bifrost base URL for the requested wire
// protocol, falling back to DefaultProtocol when protocol is empty.
func (r HarnessBifrostRouting) EndpointFor(bifrostBaseURL, protocol string) string {
	if !r.Supported {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(bifrostBaseURL), "/")
	if base == "" {
		base = DefaultBifrostURL
	}
	proto := strings.TrimSpace(protocol)
	if proto == "" {
		proto = r.DefaultProtocol
	}
	path := r.ProtocolPaths[proto]
	if path == "" && r.DefaultProtocol != "" {
		path = r.ProtocolPaths[r.DefaultProtocol]
	}
	return base + path
}

// HarnessDescriptor is the canonical metadata record for one built-in coding
// harness across LoopForge backend APIs, workflow validation, and Tauri UI.
type HarnessDescriptor struct {
	ID           string                `json:"id"`
	Kind         string                `json:"kind"`
	Name         string                `json:"name"`
	Vendor       string                `json:"vendor"`
	Binary       string                `json:"binary"`
	Category     string                `json:"category"` // general, rlm, mcp-native, ide-cli
	License      string                `json:"license,omitempty"`
	Description  string                `json:"description,omitempty"`
	InstallMode  string                `json:"installMode"` // npm, script, binary, preinstalled
	InstallHint  map[string]string     `json:"installHint,omitempty"`
	Protocols    []string              `json:"protocols,omitempty"`
	Efforts      []string              `json:"efforts,omitempty"`
	Capabilities HarnessCapabilities   `json:"capabilities"`
	Bifrost      HarnessBifrostRouting `json:"bifrost"`
}

// HarnessOverride lets a manual run or issue dispatch override the harness,
// model, reasoning effort, and Prime Agent RLM options without cloning roles.
type HarnessOverride struct {
	ExecutorID     string `json:"executorId,omitempty"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	RLMMaxDepth    int    `json:"rlmMaxDepth,omitempty"`
	RLMMaxSteps    int    `json:"rlmMaxSteps,omitempty"`
	RLMTokenBudget int    `json:"rlmTokenBudget,omitempty"`
	AutoRefine     bool   `json:"autoRefine,omitempty"`
}

// IsZero reports whether the override leaves every field unset.
func (o *HarnessOverride) IsZero() bool {
	if o == nil {
		return true
	}
	return strings.TrimSpace(o.ExecutorID) == "" &&
		strings.TrimSpace(o.Model) == "" &&
		strings.TrimSpace(o.Effort) == "" &&
		o.RLMMaxDepth == 0 &&
		o.RLMMaxSteps == 0 &&
		o.RLMTokenBudget == 0 &&
		!o.AutoRefine
}

// Validate checks override bounds and optional effort compatibility.
func (o *HarnessOverride) Validate() error {
	if o == nil {
		return nil
	}
	o.ExecutorID = strings.TrimSpace(o.ExecutorID)
	o.Model = strings.TrimSpace(o.Model)
	o.Effort = strings.TrimSpace(o.Effort)
	if o.ExecutorID != "" {
		desc, ok := HarnessByKind(o.ExecutorID)
		if !ok && o.ExecutorID != "mock" {
			return fmt.Errorf("不支持的 CLI：%s", o.ExecutorID)
		}
		if ok && o.Effort != "" {
			if err := ValidateAgentEffort(o.Effort, desc.Kind); err != nil {
				return err
			}
		}
	}
	if len(o.Model) > 256 || strings.ContainsAny(o.Model, "\r\n\x00") {
		return fmt.Errorf("模型 ID 无效")
	}
	if o.RLMMaxDepth < 0 || o.RLMMaxDepth > 16 {
		return fmt.Errorf("rlmMaxDepth must be between 0 and 16")
	}
	if o.RLMMaxSteps < 0 || o.RLMMaxSteps > 1000 {
		return fmt.Errorf("rlmMaxSteps must be between 0 and 1000")
	}
	if o.RLMTokenBudget < 0 {
		return fmt.Errorf("rlmTokenBudget must be non-negative")
	}
	return nil
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func allSandboxDrivers() []string {
	return []string{SandboxDriverLocal, SandboxDriverNative, SandboxDriverDocker}
}

func openAIAndAnthropicBifrostPaths() map[string]string {
	return map[string]string{
		AgentCLIProtocolChat:      "/v1",
		AgentCLIProtocolResponses: "/v1",
		AgentCLIProtocolMessages:  "/anthropic",
	}
}

// BuiltInHarnesses returns the complete ordered descriptor catalog for all 13
// built-in coding harnesses supported by LoopForge.
func BuiltInHarnesses() []HarnessDescriptor {
	return []HarnessDescriptor{
		{
			ID:          "codebuddy",
			Kind:        "codebuddy",
			Name:        "CodeBuddy",
			Vendor:      "Tencent",
			Binary:      "codebuddy",
			Category:    "general",
			License:     "Proprietary",
			Description: "Tencent CodeBuddy CLI with native hooks, MCP, and platform sponsor key support.",
			InstallMode: "npm",
			InstallHint: map[string]string{
				"unix":    "npm install -g @tencent-ai/codebuddy-code",
				"windows": "npm install -g @tencent-ai/codebuddy-code",
			},
			Efforts: cloneStrings(ExecutorEffortLevels["codebuddy"]),
			Capabilities: HarnessCapabilities{
				MCP:                true,
				NativeHooks:        true,
				NativeApprovalHook: true,
				StreamJSON:         true,
				SessionResume:      true,
				ReadOnlyChat:       true,
				SandboxDrivers:     allSandboxDrivers(),
			},
			Bifrost: HarnessBifrostRouting{Supported: false},
		},
		{
			ID:          "codex",
			Kind:        "codex",
			Name:        "Codex",
			Vendor:      "OpenAI",
			Binary:      "codex",
			Category:    "general",
			License:     "Apache-2.0",
			Description: "OpenAI Codex CLI supporting Responses API, sandboxed exec, hooks, and Bifrost routing.",
			InstallMode: "npm",
			InstallHint: map[string]string{
				"unix":    "npm install -g @openai/codex",
				"windows": "npm install -g @openai/codex",
			},
			Protocols: []string{AgentCLIProtocolResponses},
			Efforts:   cloneStrings(ExecutorEffortLevels["codex"]),
			Capabilities: HarnessCapabilities{
				MCP:                true,
				NativeHooks:        true,
				NativeApprovalHook: true,
				StreamJSON:         true,
				SessionResume:      true,
				ReadOnlyChat:       true,
				LocalLogin:         true,
				SandboxDrivers:     allSandboxDrivers(),
			},
			Bifrost: HarnessBifrostRouting{
				Supported:       true,
				DefaultProtocol: AgentCLIProtocolResponses,
				ProtocolPaths:   map[string]string{AgentCLIProtocolResponses: "/v1"},
				ConfigMechanism: "cli_flag",
			},
		},
		{
			ID:          "claude",
			Kind:        "claude-code",
			Name:        "Claude Code",
			Vendor:      "Anthropic",
			Binary:      "claude",
			Category:    "general",
			License:     "Proprietary",
			Description: "Anthropic Claude Code CLI with Anthropic Messages streaming, hooks, plugins, and Bifrost routing.",
			InstallMode: "npm",
			InstallHint: map[string]string{
				"unix":    "npm install -g @anthropic-ai/claude-code",
				"windows": "npm install -g @anthropic-ai/claude-code",
			},
			Protocols: []string{AgentCLIProtocolMessages},
			Efforts:   cloneStrings(ExecutorEffortLevels["claude-code"]),
			Capabilities: HarnessCapabilities{
				MCP:                true,
				NativeHooks:        true,
				NativeApprovalHook: true,
				StreamJSON:         true,
				SessionResume:      true,
				ReadOnlyChat:       true,
				LocalLogin:         true,
				SandboxDrivers:     allSandboxDrivers(),
			},
			Bifrost: HarnessBifrostRouting{
				Supported:       true,
				DefaultProtocol: AgentCLIProtocolMessages,
				ProtocolPaths:   map[string]string{AgentCLIProtocolMessages: "/anthropic"},
				ConfigMechanism: "env",
			},
		},
		{
			ID:          "prime-agent",
			Kind:        "prime-agent",
			Name:        "Prime Agent",
			Vendor:      "Prime Intellect",
			Binary:      "prime-agent",
			Category:    "rlm",
			License:     "MIT",
			Description: "Self-improving Recursive Language Model (RLM) coding & research harness with persistent IPython REPL and continual /refine.",
			InstallMode: "script",
			InstallHint: map[string]string{
				"unix":    "curl -fsSL https://app.primeintellect.ai/prime-agent/install.sh | sh",
				"windows": "irm https://app.primeintellect.ai/prime-agent/install.ps1 | iex",
			},
			Protocols: cloneStrings(AgentCLIProtocolOptions["prime-agent"]),
			Efforts:   cloneStrings(ExecutorEffortLevels["prime-agent"]),
			Capabilities: HarnessCapabilities{
				RLMLongHorizon:     true,
				PersistentREPL:     true,
				SelfRefine:         true,
				MCP:                true,
				NativeHooks:        false,
				NativeApprovalHook: false,
				StreamJSON:         true,
				SessionResume:      true,
				ReadOnlyChat:       true,
				SandboxDrivers:     allSandboxDrivers(),
			},
			Bifrost: HarnessBifrostRouting{
				Supported:       true,
				DefaultProtocol: AgentCLIProtocolChat,
				ProtocolPaths:   openAIAndAnthropicBifrostPaths(),
				ConfigMechanism: "models_json",
			},
		},
		{
			ID:          "gemini-cli",
			Kind:        "gemini-cli",
			Name:        "Gemini CLI",
			Vendor:      "Google",
			Binary:      "gemini",
			Category:    "general",
			License:     "Apache-2.0",
			Description: "Open-source Google Gemini CLI with 1M+ context, native stream-json, BeforeTool hooks, MCP, and Bifrost /genai routing.",
			InstallMode: "npm",
			InstallHint: map[string]string{
				"unix":    "npm install -g @google/gemini-cli",
				"windows": "npm install -g @google/gemini-cli",
			},
			Protocols: cloneStrings(AgentCLIProtocolOptions["gemini-cli"]),
			Efforts:   cloneStrings(ExecutorEffortLevels["gemini-cli"]),
			Capabilities: HarnessCapabilities{
				MCP:                true,
				NativeHooks:        true,
				NativeApprovalHook: true,
				StreamJSON:         true,
				SessionResume:      true,
				ReadOnlyChat:       true,
				SandboxDrivers:     allSandboxDrivers(),
			},
			Bifrost: HarnessBifrostRouting{
				Supported:       true,
				DefaultProtocol: AgentCLIProtocolGemini,
				ProtocolPaths:   map[string]string{AgentCLIProtocolGemini: "/genai"},
				ConfigMechanism: "env",
			},
		},
		{
			ID:          "goose",
			Kind:        "goose",
			Name:        "Goose",
			Vendor:      "Block",
			Binary:      "goose",
			Category:    "mcp-native",
			License:     "Apache-2.0",
			Description: "Native Rust MCP-first coding & automation agent with YAML recipes, subagents, stream-json, and multi-provider Bifrost routing.",
			InstallMode: "script",
			InstallHint: map[string]string{
				"unix":    "curl -fsSL https://github.com/block/goose/releases/download/stable/download_cli.sh | CONFIGURE=false bash",
				"windows": "irm https://github.com/block/goose/releases/download/stable/download_cli.ps1 | iex",
			},
			Protocols: cloneStrings(AgentCLIProtocolOptions["goose"]),
			Efforts:   cloneStrings(ExecutorEffortLevels["goose"]),
			Capabilities: HarnessCapabilities{
				MCP:                true,
				NativeHooks:        false,
				NativeApprovalHook: false,
				StreamJSON:         true,
				SessionResume:      true,
				ReadOnlyChat:       true,
				SandboxDrivers:     allSandboxDrivers(),
			},
			Bifrost: HarnessBifrostRouting{
				Supported:       true,
				DefaultProtocol: AgentCLIProtocolChat,
				ProtocolPaths: map[string]string{
					AgentCLIProtocolChat:     "/v1",
					AgentCLIProtocolMessages: "/anthropic",
				},
				ConfigMechanism: "env",
			},
		},
		{
			ID:          "opencode",
			Kind:        "opencode",
			Name:        "OpenCode",
			Vendor:      "OpenCode",
			Binary:      "opencode",
			Category:    "general",
			License:     "MIT",
			Description: "Multi-provider terminal coding agent with plugin hooks, MCP, and OpenAI/Anthropic gateway routing.",
			InstallMode: "npm",
			InstallHint: map[string]string{
				"unix":    "npm install -g opencode-ai",
				"windows": "npm install -g opencode-ai",
			},
			Protocols: cloneStrings(AgentCLIProtocolOptions["opencode"]),
			Efforts:   cloneStrings(ExecutorEffortLevels["opencode"]),
			Capabilities: HarnessCapabilities{
				MCP:                true,
				NativeHooks:        true,
				NativeApprovalHook: true,
				StreamJSON:         true,
				SessionResume:      true,
				ReadOnlyChat:       true,
				SandboxDrivers:     allSandboxDrivers(),
			},
			Bifrost: HarnessBifrostRouting{
				Supported:       true,
				DefaultProtocol: AgentCLIProtocolChat,
				ProtocolPaths:   openAIAndAnthropicBifrostPaths(),
				ConfigMechanism: "env",
			},
		},
		{
			ID:          "cursor",
			Kind:        "cursor",
			Name:        "Cursor CLI",
			Vendor:      "Cursor",
			Binary:      "cursor-agent",
			Category:    "ide-cli",
			License:     "Proprietary",
			Description: "Cursor headless CLI with preToolUse approval hooks, MCP, and account-plan model discovery.",
			InstallMode: "script",
			InstallHint: map[string]string{
				"unix":    "curl -fsSL https://cursor.com/install | bash",
				"windows": "irm https://cursor.com/install.ps1 | iex",
			},
			Efforts: cloneStrings(ExecutorEffortLevels["cursor"]),
			Capabilities: HarnessCapabilities{
				MCP:                true,
				NativeHooks:        true,
				NativeApprovalHook: true,
				StreamJSON:         true,
				SessionResume:      true,
				ReadOnlyChat:       true,
				SandboxDrivers:     allSandboxDrivers(),
			},
			Bifrost: HarnessBifrostRouting{Supported: false},
		},
		{
			ID:          "agy",
			Kind:        "agy",
			Name:        "Antigravity",
			Vendor:      "Google",
			Binary:      "agy",
			Category:    "general",
			License:     "Proprietary",
			Description: "Google Antigravity CLI supporting Gemini API, OpenAI-compatible gateway mode, local login, and PreToolUse hooks.",
			InstallMode: "script",
			InstallHint: map[string]string{
				"unix":    "curl -fsSL https://antigravity.google/cli/install.sh | bash",
				"windows": "curl -fsSL https://antigravity.google/cli/install.sh | bash",
			},
			Protocols: cloneStrings(AgentCLIProtocolOptions["agy"]),
			Efforts:   cloneStrings(ExecutorEffortLevels["agy"]),
			Capabilities: HarnessCapabilities{
				MCP:                true,
				NativeHooks:        true,
				NativeApprovalHook: true,
				StreamJSON:         true,
				SessionResume:      true,
				ReadOnlyChat:       true,
				LocalLogin:         true,
				SandboxDrivers:     allSandboxDrivers(),
			},
			Bifrost: HarnessBifrostRouting{
				Supported:       true,
				DefaultProtocol: AgentCLIProtocolGemini,
				ProtocolPaths: map[string]string{
					AgentCLIProtocolGemini: "/genai",
					AgentCLIProtocolChat:   "/v1",
				},
				ConfigMechanism: "env",
			},
		},
		{
			ID:          "kimi",
			Kind:        "kimi",
			Name:        "Kimi Code",
			Vendor:      "Moonshot AI",
			Binary:      "kimi",
			Category:    "general",
			License:     "Apache-2.0",
			Description: "Moonshot Kimi Code CLI with TOML provider config, PreToolUse hooks, MCP, and Bifrost routing.",
			InstallMode: "npm",
			InstallHint: map[string]string{
				"unix":    "npm install -g @moonshot-ai/kimi-code",
				"windows": "npm install -g @moonshot-ai/kimi-code",
			},
			Protocols: cloneStrings(AgentCLIProtocolOptions["kimi"]),
			Efforts:   cloneStrings(ExecutorEffortLevels["kimi"]),
			Capabilities: HarnessCapabilities{
				MCP:                true,
				NativeHooks:        true,
				NativeApprovalHook: true,
				StreamJSON:         true,
				SessionResume:      true,
				ReadOnlyChat:       true,
				SandboxDrivers:     allSandboxDrivers(),
			},
			Bifrost: HarnessBifrostRouting{
				Supported:       true,
				DefaultProtocol: AgentCLIProtocolChat,
				ProtocolPaths:   openAIAndAnthropicBifrostPaths(),
				ConfigMechanism: "toml",
			},
		},
		{
			ID:          "pi",
			Kind:        "pi",
			Name:        "Pi",
			Vendor:      "Earendil Works",
			Binary:      "pi",
			Category:    "general",
			License:     "MIT",
			Description: "Minimalist extensible coding agent with QuickJS codemode, TypeScript approval extensions, and models.json gateway routing.",
			InstallMode: "npm",
			InstallHint: map[string]string{
				"unix":    "npm install -g @earendil-works/pi-coding-agent",
				"windows": "npm install -g @earendil-works/pi-coding-agent",
			},
			Protocols: cloneStrings(AgentCLIProtocolOptions["pi"]),
			Efforts:   cloneStrings(ExecutorEffortLevels["pi"]),
			Capabilities: HarnessCapabilities{
				MCP:                true,
				NativeHooks:        true,
				NativeApprovalHook: true,
				StreamJSON:         true,
				SessionResume:      true,
				ReadOnlyChat:       true,
				SandboxDrivers:     allSandboxDrivers(),
			},
			Bifrost: HarnessBifrostRouting{
				Supported:       true,
				DefaultProtocol: AgentCLIProtocolChat,
				ProtocolPaths:   openAIAndAnthropicBifrostPaths(),
				ConfigMechanism: "models_json",
			},
		},
		{
			ID:          "dsh",
			Kind:        "dsh",
			Name:        "DeepSeek Harness",
			Vendor:      "DeepSeek",
			Binary:      "dsh",
			Category:    "general",
			License:     "MIT",
			Description: "DeepSeek headless agent harness with JSON patches, plugin approval hooks, MCP, and Bifrost routing.",
			InstallMode: "npm",
			InstallHint: map[string]string{
				"unix":    "npm install -g @deepseek-ai/dsh@0.2.2-rc.1",
				"windows": "npm install -g @deepseek-ai/dsh@0.2.2-rc.1",
			},
			Protocols: cloneStrings(AgentCLIProtocolOptions["dsh"]),
			Efforts:   cloneStrings(ExecutorEffortLevels["dsh"]),
			Capabilities: HarnessCapabilities{
				MCP:                true,
				NativeHooks:        true,
				NativeApprovalHook: true,
				StreamJSON:         true,
				SessionResume:      true,
				ReadOnlyChat:       true,
				SandboxDrivers:     allSandboxDrivers(),
			},
			Bifrost: HarnessBifrostRouting{
				Supported:       true,
				DefaultProtocol: AgentCLIProtocolChat,
				ProtocolPaths:   openAIAndAnthropicBifrostPaths(),
				ConfigMechanism: "patch",
			},
		},
		{
			ID:          "zcode",
			Kind:        "zcode",
			Name:        "ZCode",
			Vendor:      "Zhipu AI",
			Binary:      "zcode",
			Category:    "general",
			License:     "Proprietary",
			Description: "Zhipu ZCode CLI supporting BigModel Anthropic-compatible and custom gateway endpoints.",
			InstallMode: "preinstalled",
			Protocols:   cloneStrings(AgentCLIProtocolOptions["zcode"]),
			Efforts:     cloneStrings(ExecutorEffortLevels["zcode"]),
			Capabilities: HarnessCapabilities{
				MCP:                true,
				NativeHooks:        true,
				NativeApprovalHook: true,
				StreamJSON:         true,
				SessionResume:      true,
				ReadOnlyChat:       true,
				SandboxDrivers:     allSandboxDrivers(),
			},
			Bifrost: HarnessBifrostRouting{
				Supported:       true,
				DefaultProtocol: AgentCLIProtocolMessages,
				ProtocolPaths:   openAIAndAnthropicBifrostPaths(),
				ConfigMechanism: "models_json",
			},
		},
	}
}

// HarnessByKind resolves a built-in harness descriptor by either Kind
// ("claude-code") or legacy Executor ID ("claude"), case-insensitively.
func HarnessByKind(kind string) (HarnessDescriptor, bool) {
	needle := strings.TrimSpace(kind)
	if needle == "" {
		return HarnessDescriptor{}, false
	}
	for _, h := range BuiltInHarnesses() {
		if strings.EqualFold(h.Kind, needle) || strings.EqualFold(h.ID, needle) {
			return h, true
		}
	}
	return HarnessDescriptor{}, false
}
