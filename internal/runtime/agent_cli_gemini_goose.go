package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	geminiCLISettingsPath    = ".gemini/settings.json"
	gooseConfigPath          = ".config/goose/config.yaml"
	gooseInstallerURL        = "https://github.com/block/goose/releases/download/stable/download_cli.sh"
	gooseWindowsInstallerURL = "https://github.com/block/goose/releases/download/stable/download_cli.ps1"
	gooseInstallScript       = "curl -fsSL --proto '=https' --tlsv1.2 " + gooseInstallerURL + " | CONFIGURE=false bash"
	gooseWinInstallScript    = "irm " + gooseWindowsInstallerURL + " | iex"
)

var geminiCLIHostEnv = []string{
	"GEMINI_SYSTEM_MD",
	"GEMINI_CLI_HOME",
	"GOOGLE_CLOUD_PROJECT",
	"GOOGLE_CLOUD_LOCATION",
}

var gooseHostEnv = []string{
	"GOOSE_PROVIDER",
	"GOOSE_MODEL",
	"GOOSE_MODE",
	"GOOSE_TEMPERATURE",
	"GOOSE_MAX_TURNS",
	"OPENAI_HOST",
	"OPENAI_BASE_PATH",
	"ANTHROPIC_HOST",
	"XDG_CONFIG_HOME",
	"XDG_DATA_HOME",
}

// writeGeminiCLIMCPConfig merges mcpServers into ~/.gemini/settings.json
// without discarding BeforeTool approval hooks installed for conversation mode.
func writeGeminiCLIMCPConfig(home AgentCLIHome, entries map[string]any, options AgentCLIMCPOptions) error {
	document := map[string]any{}
	root, err := openAgentCLIHome(home)
	if err != nil {
		return err
	}
	data, readErr := root.ReadFile(geminiCLISettingsPath)
	root.Close()
	if readErr == nil {
		_ = json.Unmarshal(data, &document)
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return readErr
	}
	servers := map[string]any{}
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		entry, _ := entries[id].(map[string]any)
		if entry == nil {
			continue
		}
		if u, _ := entry["url"].(string); u != "" {
			server := map[string]any{"httpUrl": u, "timeout": 600000}
			if headers := entry["headers"]; headers != nil {
				server["headers"] = headers
			}
			servers[id] = server
			continue
		}
		if command, _ := entry["command"].(string); command != "" {
			server := map[string]any{"command": command, "timeout": 600000}
			for _, key := range []string{"args", "env", "cwd"} {
				if value := entry[key]; value != nil {
					server[key] = value
				}
			}
			servers[id] = server
		}
	}
	document["mcpServers"] = servers
	if options.ReadOnly {
		document["tools"] = map[string]any{"exclude": []string{"run_shell_command", "write_file", "replace", "web_fetch"}}
	}
	out, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return writeAgentCLIFile(home, geminiCLISettingsPath, out)
}

// installGeminiCLIApproval merges LoopForge's BeforeTool command hook into
// ~/.gemini/settings.json while preserving mcpServers.
func installGeminiCLIApproval(home AgentCLIHome, launch string) error {
	document := map[string]any{}
	root, err := openAgentCLIHome(home)
	if err != nil {
		return err
	}
	data, readErr := root.ReadFile(geminiCLISettingsPath)
	root.Close()
	if readErr == nil {
		if err := json.Unmarshal(data, &document); err != nil {
			return err
		}
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return readErr
	}
	hooks, _ := document["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	hooks["BeforeTool"] = []any{
		map[string]any{
			"matcher": ".*",
			"hooks": []any{
				map[string]any{
					"name":    "loopforge-approval",
					"type":    "command",
					"command": launch + " gemini",
					"timeout": conversationApprovalHookWatchdog,
				},
			},
		},
	}
	document["hooks"] = hooks
	document["hooksConfig"] = map[string]any{"enabled": true}
	out, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return writeAgentCLIFile(home, geminiCLISettingsPath, out)
}

// applyGeminiCLICredential binds GEMINI_API_KEY, optional Bifrost /genai
// GOOGLE_GEMINI_BASE_URL, and GEMINI_SYSTEM_MD for @google/gemini-cli.
func applyGeminiCLICredential(key, baseURL string, options AgentCLIOptions, argv []string) ([]string, map[string]string, error) {
	env := map[string]string{
		"GEMINI_API_KEY": key,
	}
	if baseURL != "" {
		env["GOOGLE_GEMINI_BASE_URL"] = baseURL
		env["GEMINI_BASE_URL"] = baseURL
	}
	if prompt := strings.TrimSpace(options.SystemPromptFile); prompt != "" {
		env["GEMINI_SYSTEM_MD"] = prompt
	}
	return argv, env, nil
}

// geminiCLIStream translates `gemini -p ... --output-format stream-json` NDJSON
// events (init, message, tool_use, tool_result, error, result) into ClaudeStream.
type geminiCLIStream struct {
	segment int
	text    strings.Builder
	failure string
}

func (g *geminiCLIStream) flushText(s *ClaudeStream) []string {
	if g.text.Len() == 0 {
		return nil
	}
	text := g.text.String()
	g.text.Reset()
	g.segment++
	return s.Text(text)
}

func (g *geminiCLIStream) translate(s *ClaudeStream, raw map[string]any) []string {
	switch stringValue(raw["type"]) {
	case "init":
		return s.Init(stringValue(raw["session_id"]), stringValue(raw["model"]))
	case "message":
		if stringValue(raw["role"]) != "assistant" {
			return nil
		}
		content := stringValue(raw["content"])
		if content == "" {
			return nil
		}
		if delta, _ := raw["delta"].(bool); delta {
			g.text.WriteString(content)
			return s.Delta(fmt.Sprintf("text#%d", g.segment), "text", content)
		}
		g.text.Reset()
		g.segment++
		return s.Text(content)
	case "tool_use":
		out := g.flushText(s)
		id := stringValue(raw["tool_id"])
		if id == "" {
			id = stringValue(raw["id"])
		}
		name := stringValue(raw["tool_name"])
		if name == "" {
			name = stringValue(raw["name"])
		}
		params := raw["parameters"]
		if params == nil {
			params = raw["args"]
		}
		return append(out, s.ToolUse(id, name, params)...)
	case "tool_result":
		out := g.flushText(s)
		id := stringValue(raw["tool_id"])
		if id == "" {
			id = stringValue(raw["id"])
		}
		status := strings.ToLower(stringValue(raw["status"]))
		failed := status == "error" || status == "failed"
		body := stringValue(raw["output"])
		if body == "" && failed {
			body = errorText(raw["error"])
		}
		return append(out, s.ToolResult(id, failed, body)...)
	case "error":
		msg := errorText(raw["error"])
		if msg == "" {
			msg = stringValue(raw["message"])
		}
		if msg != "" {
			g.failure = msg
		}
		if stringValue(raw["severity"]) == "error" || stringValue(raw["severity"]) == "" {
			if msg == "" {
				msg = "Gemini CLI 运行失败"
			}
			return append(g.flushText(s), s.Result(msg, true)...)
		}
	case "result":
		out := g.flushText(s)
		if stats, ok := raw["stats"].(map[string]any); ok {
			s.SetUsage(
				int64Value(stats["input_tokens"]),
				int64Value(stats["output_tokens"]),
				int64Value(stats["cached"]),
				0,
				int64Value(stats["thoughts_tokens"]),
			)
		}
		status := strings.ToLower(stringValue(raw["status"]))
		failed := status != "" && status != "success"
		text := stringValue(raw["response"])
		if failed && text == "" {
			if text = errorText(raw["error"]); text == "" {
				text = g.failure
			}
			if text == "" {
				text = "Gemini CLI 运行失败"
			}
		}
		return append(out, s.Result(text, failed)...)
	}
	return nil
}

// writeGooseMCPConfig writes deterministic YAML extensions configuration to
// ~/.config/goose/config.yaml for Block's Goose CLI.
func writeGooseMCPConfig(home AgentCLIHome, entries map[string]any, options AgentCLIMCPOptions) error {
	var b strings.Builder
	b.WriteString("extensions:\n")
	b.WriteString("  developer:\n")
	b.WriteString("    name: developer\n")
	b.WriteString("    type: builtin\n")
	if options.ReadOnly {
		b.WriteString("    enabled: false\n")
	} else {
		b.WriteString("    enabled: true\n")
	}
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		entry, _ := entries[id].(map[string]any)
		if entry == nil {
			continue
		}
		key := safeID(id)
		if key == "" {
			continue
		}
		if u, _ := entry["url"].(string); u != "" {
			b.WriteString("  " + key + ":\n")
			b.WriteString("    name: " + strconv.Quote(id) + "\n")
			b.WriteString("    type: streamable_http\n")
			b.WriteString("    uri: " + strconv.Quote(u) + "\n")
			b.WriteString("    enabled: true\n")
			b.WriteString("    timeout: 600\n")
			if headers, ok := entry["headers"].(map[string]any); ok && len(headers) > 0 {
				b.WriteString("    headers:\n")
				hKeys := make([]string, 0, len(headers))
				for k := range headers {
					hKeys = append(hKeys, k)
				}
				sort.Strings(hKeys)
				for _, k := range hKeys {
					b.WriteString("      " + strconv.Quote(k) + ": " + strconv.Quote(fmt.Sprint(headers[k])) + "\n")
				}
			}
			continue
		}
		if command, _ := entry["command"].(string); command != "" {
			b.WriteString("  " + key + ":\n")
			b.WriteString("    name: " + strconv.Quote(id) + "\n")
			b.WriteString("    type: stdio\n")
			b.WriteString("    cmd: " + strconv.Quote(command) + "\n")
			b.WriteString("    enabled: true\n")
			b.WriteString("    timeout: 600\n")
			if args := sliceValue(entry["args"]); len(args) > 0 {
				b.WriteString("    args:\n")
				for _, arg := range args {
					b.WriteString("      - " + strconv.Quote(fmt.Sprint(arg)) + "\n")
				}
			} else if strArgs, ok := entry["args"].([]string); ok && len(strArgs) > 0 {
				b.WriteString("    args:\n")
				for _, arg := range strArgs {
					b.WriteString("      - " + strconv.Quote(arg) + "\n")
				}
			}
			if envMap, ok := entry["env"].(map[string]any); ok && len(envMap) > 0 {
				b.WriteString("    envs:\n")
				eKeys := make([]string, 0, len(envMap))
				for k := range envMap {
					eKeys = append(eKeys, k)
				}
				sort.Strings(eKeys)
				for _, k := range eKeys {
					b.WriteString("      " + strconv.Quote(k) + ": " + strconv.Quote(fmt.Sprint(envMap[k])) + "\n")
				}
			} else if envStr, ok := entry["env"].(map[string]string); ok && len(envStr) > 0 {
				b.WriteString("    envs:\n")
				eKeys := make([]string, 0, len(envStr))
				for k := range envStr {
					eKeys = append(eKeys, k)
				}
				sort.Strings(eKeys)
				for _, k := range eKeys {
					b.WriteString("      " + strconv.Quote(k) + ": " + strconv.Quote(envStr[k]) + "\n")
				}
			}
		}
	}
	return writeAgentCLIFile(home, gooseConfigPath, []byte(b.String()))
}

// splitGooseOpenAIBaseURL separates an OpenAI-compatible gateway URL (such as
// Bifrost http://127.0.0.1:8091/v1 or https://gw.example.com/openai/v1) into
// Goose's OPENAI_HOST (scheme://host) and OPENAI_BASE_PATH (subpath ending in
// chat/completions).
func splitGooseOpenAIBaseURL(raw string) (host, basePath string) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", "v1/chat/completions"
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return trimmed, "v1/chat/completions"
	}
	host = u.Scheme + "://" + u.Host
	p := strings.Trim(u.Path, "/")
	switch {
	case p == "":
		basePath = "v1/chat/completions"
	case strings.HasSuffix(p, "/chat/completions") || p == "chat/completions":
		basePath = p
	default:
		basePath = p + "/chat/completions"
	}
	return host, basePath
}

// applyGooseCredential configures Block Goose's provider, model, autonomous
// mode, XDG private directories, and Bifrost OpenAI/Anthropic routing vars.
func applyGooseCredential(homeEnv, key, baseURL, protocol, model string, options AgentCLIOptions, argv []string) ([]string, map[string]string, error) {
	mode := "auto"
	if options.ReadOnly {
		mode = "chat"
	}
	env := map[string]string{
		"XDG_CONFIG_HOME": homeEnv + "/.config",
		"XDG_DATA_HOME":   homeEnv + "/.local/share",
		"GOOSE_MODE":      mode,
		"GOOSE_MODEL":     model,
	}
	if protocol == AgentCLIProtocolMessages {
		env["GOOSE_PROVIDER"] = "anthropic"
		env["ANTHROPIC_API_KEY"] = key
		if baseURL != "" {
			host := strings.TrimRight(baseURL, "/")
			host = strings.TrimSuffix(host, "/v1")
			env["ANTHROPIC_HOST"] = host
		}
	} else {
		env["GOOSE_PROVIDER"] = "openai"
		env["OPENAI_API_KEY"] = key
		if baseURL != "" {
			host, basePath := splitGooseOpenAIBaseURL(baseURL)
			env["OPENAI_HOST"] = host
			env["OPENAI_BASE_PATH"] = basePath
		}
	}
	return argv, env, nil
}

// gooseStream translates `goose run --output-format stream-json` NDJSON events
// into ClaudeStream. Handles both Goose's `.kind` event envelope
// (`message-start`, `block`, `usage`, `error`, `message-end`) and its `.type`
// message/notification/complete stream-json variant.
type gooseStream struct {
	started bool
	segment int
	failure string
}

func (g *gooseStream) translate(s *ClaudeStream, raw map[string]any) []string {
	var out []string
	if !g.started {
		session := stringValue(raw["session_id"])
		if session == "" {
			session = stringValue(raw["sessionId"])
		}
		if session != "" {
			g.started = true
			out = append(out, s.Init(session, stringValue(raw["model"]))...)
		}
	}
	kind := stringValue(raw["kind"])
	if kind == "" {
		kind = stringValue(raw["type"])
	}
	switch kind {
	case "message-start", "session":
		if !g.started {
			g.started = true
			out = append(out, s.Init(stringValue(raw["session_id"]), stringValue(raw["model"]))...)
		}
		return out
	case "block":
		block, _ := raw["block"].(map[string]any)
		if block == nil {
			block = raw
		}
		switch stringValue(block["type"]) {
		case "text":
			if text := stringValue(block["text"]); text != "" {
				g.segment++
				out = append(out, s.Text(text)...)
			}
		case "thinking":
			if text := stringValue(block["thinking"]); text != "" {
				out = append(out, s.Thinking(text)...)
			} else if text := stringValue(block["text"]); text != "" {
				out = append(out, s.Thinking(text)...)
			}
		case "tool_use", "toolRequest":
			id := stringValue(block["id"])
			name := stringValue(block["name"])
			input := block["input"]
			if input == nil {
				input = block["arguments"]
			}
			out = append(out, s.ToolUse(id, name, input)...)
		case "tool_result", "toolResponse":
			id := stringValue(block["tool_use_id"])
			if id == "" {
				id = stringValue(block["id"])
			}
			failed, _ := block["is_error"].(bool)
			out = append(out, s.ToolResult(id, failed, contentText(block["content"]))...)
		}
		return out
	case "message":
		msg, _ := raw["message"].(map[string]any)
		if msg == nil {
			return out
		}
		role := stringValue(msg["role"])
		for _, item := range sliceValue(msg["content"]) {
			part, _ := item.(map[string]any)
			switch stringValue(part["type"]) {
			case "text":
				if role == "assistant" {
					out = append(out, s.Text(stringValue(part["text"]))...)
				}
			case "thinking":
				out = append(out, s.Thinking(stringValue(part["thinking"]))...)
			case "toolRequest":
				call, _ := part["toolCall"].(map[string]any)
				val, _ := call["value"].(map[string]any)
				out = append(out, s.ToolUse(stringValue(part["id"]), stringValue(val["name"]), val["arguments"])...)
			case "toolResponse":
				res, _ := part["toolResult"].(map[string]any)
				failed := stringValue(res["status"]) == "error"
				out = append(out, s.ToolResult(stringValue(part["id"]), failed, contentText(res["value"]))...)
			}
		}
		return out
	case "usage":
		usage, _ := raw["usage"].(map[string]any)
		if usage == nil {
			usage = raw
		}
		s.AddUsage(
			int64Value(usage["input_tokens"]),
			int64Value(usage["output_tokens"]),
			int64Value(usage["cache_read_input_tokens"]),
			int64Value(usage["cache_creation_input_tokens"]),
			int64Value(usage["reasoning_tokens"]),
		)
		return out
	case "error":
		msg := errorText(raw["error"])
		if msg == "" {
			msg = stringValue(raw["message"])
		}
		if msg == "" {
			msg = "Goose 运行失败"
		}
		g.failure = msg
		return append(out, s.Result(msg, true)...)
	case "message-end", "complete":
		if tokens, ok := raw["total_tokens"].(float64); ok && tokens > 0 && s.usage == nil {
			s.AddUsage(0, int64(tokens), 0, 0, 0)
		}
		return append(out, s.Result("", g.failure != "" && s.LastText == "")...)
	}
	return out
}
