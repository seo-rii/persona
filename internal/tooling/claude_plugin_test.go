package tooling

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersonaClaudePluginManifestDeclaresExpectedPaths(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "persona-claude-plugin", ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatalf("read plugin manifest: %v", err)
	}

	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("unmarshal plugin manifest: %v", err)
	}

	if got, want := manifest["name"], "persona-claude"; got != want {
		t.Fatalf("unexpected plugin name: got %v want %q", got, want)
	}
	if got, want := manifest["agents"], "./agents/"; got != want {
		t.Fatalf("unexpected plugin agents path: got %v want %q", got, want)
	}
	if got, want := manifest["hooks"], "./hooks/hooks.json"; got != want {
		t.Fatalf("unexpected plugin hooks path: got %v want %q", got, want)
	}

	userConfig, ok := manifest["userConfig"].(map[string]any)
	if !ok {
		t.Fatalf("expected userConfig in plugin manifest, got %#v", manifest["userConfig"])
	}
	personaBin, ok := userConfig["PERSONA_BIN"].(map[string]any)
	if !ok {
		t.Fatalf("expected PERSONA_BIN userConfig, got %#v", userConfig["PERSONA_BIN"])
	}
	description, _ := personaBin["description"].(string)
	if !strings.Contains(description, "persona binary") {
		t.Fatalf("expected persona binary description, got %q", description)
	}
}

func TestPersonaClaudePluginHooksWrapBashAndBlockWrites(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "persona-claude-plugin", "hooks", "hooks.json"))
	if err != nil {
		t.Fatalf("read hooks config: %v", err)
	}

	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("unmarshal hooks config: %v", err)
	}

	hooks, ok := config["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("expected hooks object, got %#v", config["hooks"])
	}
	preToolUse, ok := hooks["PreToolUse"].([]any)
	if !ok || len(preToolUse) != 2 {
		t.Fatalf("expected two PreToolUse handlers, got %#v", hooks["PreToolUse"])
	}

	matchers := map[string]bool{}
	for _, raw := range preToolUse {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("unexpected hook entry: %#v", raw)
		}
		matcher, _ := entry["matcher"].(string)
		matchers[matcher] = true
		hookList, ok := entry["hooks"].([]any)
		if !ok || len(hookList) != 1 {
			t.Fatalf("unexpected hook list for %q: %#v", matcher, entry["hooks"])
		}
		handler, ok := hookList[0].(map[string]any)
		if !ok {
			t.Fatalf("unexpected handler for %q: %#v", matcher, hookList[0])
		}
		if got, want := handler["command"], "${CLAUDE_PLUGIN_ROOT}/bin/persona-wrap"; got != want {
			t.Fatalf("unexpected handler command for %q: got %v want %q", matcher, got, want)
		}
	}

	for _, matcher := range []string{"Bash", "Edit|MultiEdit|Write"} {
		if !matchers[matcher] {
			t.Fatalf("missing matcher %q in hooks config", matcher)
		}
	}
}

func TestPersonaWrapRewritesBashCommandsAndPreservesInput(t *testing.T) {
	repoRoot := repoRoot(t)
	pluginData := filepath.Join(t.TempDir(), "plugin-data")
	personaStub := filepath.Join(t.TempDir(), "persona")
	if err := os.WriteFile(personaStub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write persona stub: %v", err)
	}
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id":      "session-123",
		"cwd":             repoRoot,
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"tool_input": map[string]any{
			"command":           "npm test",
			"description":       "Run tests",
			"timeout":           45,
			"run_in_background": true,
		},
	}, map[string]string{
		"CLAUDE_PLUGIN_DATA":               pluginData,
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub,
		"CLAUDE_PROJECT_DIR":               repoRoot,
	})

	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("unmarshal wrapper response: %v\n%s", err, output)
	}
	hookOutput := response["hookSpecificOutput"].(map[string]any)
	if got, want := hookOutput["permissionDecision"], "allow"; got != want {
		t.Fatalf("unexpected permission decision: got %v want %q", got, want)
	}
	updatedInput := hookOutput["updatedInput"].(map[string]any)
	if got, want := updatedInput["description"], "Run tests"; got != want {
		t.Fatalf("expected description to be preserved: got %v want %q", got, want)
	}
	if got := int(updatedInput["timeout"].(float64)); got != 45 {
		t.Fatalf("expected timeout to be preserved, got %d", got)
	}
	if got, ok := updatedInput["run_in_background"].(bool); !ok || !got {
		t.Fatalf("expected run_in_background to be preserved, got %#v", updatedInput["run_in_background"])
	}
	command := updatedInput["command"].(string)
	if !strings.Contains(command, personaStub+" --patch ") {
		t.Fatalf("expected wrapped command to invoke persona, got %q", command)
	}
	if !strings.Contains(command, " -- bash -lc 'npm test'") {
		t.Fatalf("expected wrapped command to run original Bash payload, got %q", command)
	}
	if !strings.Contains(command, filepath.Join(pluginData, "patches", "session-123-")) {
		t.Fatalf("expected wrapped command to include plugin data patch path, got %q", command)
	}
	if !strings.Contains(command, "session-123-") || !strings.Contains(command, ".patch") {
		t.Fatalf("expected wrapped command to include a session patch name, got %q", command)
	}
}

func TestPersonaWrapBypassesGitCommands(t *testing.T) {
	repoRoot := repoRoot(t)
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id":      "session-123",
		"cwd":             repoRoot,
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"tool_input": map[string]any{
			"command": "bash -lc 'git status --short'",
		},
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": "/opt/persona",
		"CLAUDE_PROJECT_DIR":               repoRoot,
	})
	if len(bytes.TrimSpace(output)) != 0 {
		t.Fatalf("expected git command to bypass persona wrapping, got %s", output)
	}
}

func TestPersonaWrapDeniesDirectWriteTools(t *testing.T) {
	repoRoot := repoRoot(t)
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id":      "session-123",
		"cwd":             repoRoot,
		"hook_event_name": "PreToolUse",
		"tool_name":       "Write",
		"tool_input": map[string]any{
			"file_path": "hello.txt",
		},
	}, nil)

	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("unmarshal wrapper response: %v\n%s", err, output)
	}
	hookOutput := response["hookSpecificOutput"].(map[string]any)
	if got, want := hookOutput["permissionDecision"], "deny"; got != want {
		t.Fatalf("unexpected permission decision: got %v want %q", got, want)
	}
	reason := hookOutput["permissionDecisionReason"].(string)
	if !strings.Contains(reason, "direct file writes") || !strings.Contains(reason, "persona") {
		t.Fatalf("unexpected deny reason: %q", reason)
	}
}

func TestReadmeDocumentsClaudePluginSupport(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"## Claude Code Plugin",
		"claude --plugin-dir ./persona-claude-plugin",
		"`settings.json` sets `\"agent\": \"persona-worker\"`",
		"`Edit`, `MultiEdit`, and `Write` are denied",
		"Codex does not currently support the same transparent Bash rewrite flow",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("README missing Claude plugin guidance %q:\n%s", want, text)
		}
	}
}

func TestPersonaClaudePluginReadmeDocumentsBehaviorAndLimits(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "persona-claude-plugin", "README.md"))
	if err != nil {
		t.Fatalf("read plugin README: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"claude --plugin-dir ./persona-claude-plugin",
		"`${CLAUDE_PLUGIN_DATA}/patches/`",
		"`Edit`, `MultiEdit`, and `Write` are denied",
		"`Read` observes the checkout on disk",
		"Commands that resolve to `git`, `gh`, `persona`, or `claude` are bypassed",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("plugin README missing %q:\n%s", want, text)
		}
	}
}

func runPersonaWrap(t *testing.T, repoRoot string, event map[string]any, env map[string]string) []byte {
	t.Helper()
	path := filepath.Join(repoRoot, "persona-claude-plugin", "bin", "persona-wrap")
	cmd := exec.Command(path)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CLAUDE_PROJECT_DIR="+repoRoot)
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	input, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal wrapper input: %v", err)
	}
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("persona-wrap failed: %v\n%s", err, out)
	}
	return out
}
