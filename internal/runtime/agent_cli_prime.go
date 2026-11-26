package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

const (
	primeAgentDirRel              = ".prime/agent"
	primeAgentSettingsPath        = ".prime/agent/settings.json"
	primeAgentModelsPath          = ".prime/agent/models.json"
	primeAgentApprovalExtPath     = ".prime/agent/extensions/loopforge-approval.ts"
	primeAgentInstallerURL        = "https://app.primeintellect.ai/prime-agent/install.sh"
	primeAgentWindowsInstallerURL = "https://app.primeintellect.ai/prime-agent/install.ps1"
	primeAgentInstallScript       = "curl -fsSL --proto '=https' --tlsv1.2 " + primeAgentInstallerURL + " | sh"
	primeAgentWinInstallScript    = "irm " + primeAgentWindowsInstallerURL + " | iex"
)

// primeAgentHostEnv are host environment variables scrubbed before launching
// Prime Agent so local sessions or host keys never leak into sandboxed runs.
var primeAgentHostEnv = []string{
	"PRIME_AGENT_DIR",
	"PRIME_API_KEY",
	"PRIME_INTELLECT_API_KEY",
	"RLM_MAX_DEPTH",
	"RLM_MODEL",
	"RLM_MAX_STEPS",
	"RLM_TOKEN_BUDGET",
}

// writePrimeAgentMCPConfig writes user-level MCP server bindings into
// ~/.prime/agent/settings.json while preserving existing keys. Prime Agent
// restricts MCP server registration to user-level settings for security.
func writePrimeAgentMCPConfig(home AgentCLIHome, entries map[string]any, options AgentCLIMCPOptions) error {
	document := map[string]any{}
	root, err := openAgentCLIHome(home)
	if err != nil {
		return err
	}
	data, readErr := root.ReadFile(primeAgentSettingsPath)
	root.Close()
	if readErr == nil {
		_ = json.Unmarshal(data, &document)
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return readErr
	}
	servers := map[string]any{}
	for id, raw := range entries {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			continue
		}
		if url, _ := entry["url"].(string); url != "" {
			server := map[string]any{"url": url}
			if headers := entry["headers"]; headers != nil {
				server["headers"] = headers
			}
			servers[id] = server
			continue
		}
		if command, _ := entry["command"].(string); command != "" {
			server := map[string]any{"command": command}
			for _, key := range []string{"args", "env", "cwd"} {
				if value := entry[key]; value != nil {
					server[key] = value
				}
			}
			servers[id] = server
		}
	}
	document["mcpServers"] = servers
	document["autoupdate"] = false
	if options.ReadOnly {
		document["permissions"] = map[string]any{
			"bash":  "deny",
			"edit":  "deny",
			"write": "deny",
			"repl":  "deny",
		}
	} else {
		delete(document, "permissions")
	}
	out, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return writeAgentCLIFile(home, primeAgentSettingsPath, out)
}

// applyPrimeAgentCredential writes Prime Agent's Pi-lineage models.json under
// the private HOME, sets PRIME_AGENT_DIR and RLM runtime limits, and exports
// standard gateway variables so nested IPython RLM helpers route via Bifrost.
func applyPrimeAgentCredential(home AgentCLIHome, homeEnv, key, baseURL, protocol, model string, options AgentCLIOptions, argv []string) ([]string, map[string]string, error) {
	api := agentCLIPiAPI(protocol)
	if api == "" {
		api = "openai-completions"
	}
	modelsDoc, err := json.MarshalIndent(map[string]any{
		"providers": map[string]any{
			"agentflow": map[string]any{
				"baseUrl": baseURL,
				"api":     api,
				"apiKey":  "$" + agentCLIKeyEnv,
				"models":  []map[string]any{{"id": model, "reasoning": true}},
			},
		},
	}, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	if err := writeAgentCLIFile(home, primeAgentModelsPath, modelsDoc); err != nil {
		return nil, nil, err
	}
	env := map[string]string{
		agentCLIKeyEnv:    key,
		"PRIME_AGENT_DIR": homeEnv + "/" + primeAgentDirRel,
	}
	if model != "" {
		env["RLM_MODEL"] = "agentflow/" + model
	}
	if options.RLMMaxDepth > 0 {
		env["RLM_MAX_DEPTH"] = strconv.Itoa(options.RLMMaxDepth)
	}
	if options.RLMMaxSteps > 0 {
		env["RLM_MAX_STEPS"] = strconv.Itoa(options.RLMMaxSteps)
	}
	if options.RLMTokenBudget > 0 {
		env["RLM_TOKEN_BUDGET"] = strconv.Itoa(options.RLMTokenBudget)
	}
	if baseURL != "" {
		if protocol == AgentCLIProtocolMessages {
			env["ANTHROPIC_BASE_URL"] = baseURL
			env["ANTHROPIC_API_KEY"] = key
		} else {
			env["OPENAI_BASE_URL"] = baseURL
			env["OPENAI_API_KEY"] = key
		}
	}
	return argv, env, nil
}

// primeAgentStream translates `prime-agent --mode json` NDJSON events into
// LoopForge's ClaudeStream representation. It handles both Pi-lineage base
// events (session, turn_start, message_update, message_end, tool_execution_*,
// agent_end) and Prime Agent RLM/REPL/refine events.
type primeAgentStream struct {
	base    piStream
	replSeq int
}

func (p *primeAgentStream) translate(s *ClaudeStream, raw map[string]any) []string {
	switch stringValue(raw["type"]) {
	case "rlm_spawn", "subagent_start":
		id := stringValue(raw["subagentId"])
		if id == "" {
			id = stringValue(raw["callId"])
		}
		if id == "" {
			id = stringValue(raw["id"])
		}
		if id == "" {
			p.replSeq++
			id = fmt.Sprintf("rlm-%d", p.replSeq)
		}
		input := map[string]any{}
		if task := stringValue(raw["task"]); task != "" {
			input["task"] = task
		}
		if prompt := stringValue(raw["prompt"]); prompt != "" {
			input["prompt"] = prompt
		}
		if depth := int64Value(raw["depth"]); depth > 0 {
			input["depth"] = depth
		}
		if model := stringValue(raw["model"]); model != "" {
			input["model"] = model
		}
		return s.ToolUse(id, "rlm.spawn", input)
	case "rlm_complete", "subagent_end":
		id := stringValue(raw["subagentId"])
		if id == "" {
			id = stringValue(raw["callId"])
		}
		if id == "" {
			id = stringValue(raw["id"])
		}
		failed, _ := raw["isError"].(bool)
		if stringValue(raw["status"]) == "error" || stringValue(raw["status"]) == "failed" {
			failed = true
		}
		output := stringValue(raw["result"])
		if output == "" {
			output = contentText(raw["output"])
		}
		if output == "" && failed {
			output = errorText(raw["error"])
		}
		if usage, ok := raw["usage"].(map[string]any); ok {
			s.AddUsage(int64Value(usage["input"]), int64Value(usage["output"]), int64Value(usage["cacheRead"]), int64Value(usage["cacheWrite"]), int64Value(usage["reasoning"]))
		}
		return s.ToolResult(id, failed, output)
	case "repl_exec", "repl_start":
		id := stringValue(raw["callId"])
		if id == "" {
			id = stringValue(raw["cellId"])
		}
		if id == "" {
			p.replSeq++
			id = fmt.Sprintf("repl-%d", p.replSeq)
		}
		return s.ToolUse(id, "ipython_repl", map[string]any{"code": stringValue(raw["code"])})
	case "repl_output", "repl_result":
		id := stringValue(raw["callId"])
		if id == "" {
			id = stringValue(raw["cellId"])
		}
		failed, _ := raw["isError"].(bool)
		if stringValue(raw["status"]) == "error" {
			failed = true
		}
		out := stringValue(raw["stdout"])
		if stderr := stringValue(raw["stderr"]); stderr != "" {
			if out != "" {
				out += "\n"
			}
			out += stderr
		}
		if out == "" {
			out = contentText(raw["output"])
		}
		return s.ToolResult(id, failed, out)
	case "refine_applied":
		summary := stringValue(raw["summary"])
		if summary == "" {
			summary = "Continual harness /refine snapshot applied"
		}
		return s.Thinking("[prime-agent /refine] " + summary)
	case "error":
		msg := errorText(raw["error"])
		if msg == "" {
			msg = stringValue(raw["message"])
		}
		if msg == "" {
			msg = "Prime Agent 运行失败"
		}
		p.base.failure = msg
		return s.Result(msg, true)
	default:
		return p.base.translate(s, raw)
	}
}
