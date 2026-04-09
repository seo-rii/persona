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
	for _, key := range []string{
		"DAEMON_BASE_MODE",
		"DAEMON_BASE_REF",
		"DAEMON_ALLOW_DIRTY",
		"DAEMON_IGNORED_MODE",
		"DAEMON_IGNORED_MAX",
		"DAEMON_APPLY_MODE",
	} {
		entry, ok := userConfig[key].(map[string]any)
		if !ok {
			t.Fatalf("expected %s userConfig entry, got %#v", key, userConfig[key])
		}
		description, _ := entry["description"].(string)
		if !strings.Contains(strings.ToLower(description), "daemon") {
			t.Fatalf("expected %s description to mention daemon, got %q", key, description)
		}
	}
}

func TestPersonaClaudePluginHooksWrapBashAndRouteFileTools(t *testing.T) {
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
	postToolUse, ok := hooks["PostToolUse"].([]any)
	if !ok || len(postToolUse) != 1 {
		t.Fatalf("expected one PostToolUse handler, got %#v", hooks["PostToolUse"])
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

	for _, matcher := range []string{"Bash", "Read|Edit|MultiEdit|Write|Glob|Grep"} {
		if !matchers[matcher] {
			t.Fatalf("missing matcher %q in hooks config", matcher)
		}
	}
	postEntry := postToolUse[0].(map[string]any)
	if got, want := postEntry["matcher"], "Read|Edit|MultiEdit|Write|Glob|Grep"; got != want {
		t.Fatalf("unexpected PostToolUse matcher: got %v want %q", got, want)
	}
}

func TestPersonaWrapRewritesBashCommandsAndPreservesInput(t *testing.T) {
	repoRoot := repoRoot(t)
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
			"shell":             "/bin/zsh",
			"description":       "Run tests",
			"timeout":           45,
			"run_in_background": true,
		},
	}, map[string]string{
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
	if !strings.Contains(command, personaStub+" daemon exec --session-key session-123 -- ") {
		t.Fatalf("expected wrapped command to invoke persona daemon exec, got %q", command)
	}
	if !strings.Contains(command, " -- /bin/zsh -lc 'npm test'") {
		t.Fatalf("expected wrapped command to run original payload through selected shell, got %q", command)
	}
}

func TestPersonaWrapForwardsConfiguredDaemonOptions(t *testing.T) {
	repoRoot := repoRoot(t)
	logPath := filepath.Join(t.TempDir(), "persona.log")
	personaStub := writePersonaDaemonStub(t, filepath.Join(t.TempDir(), "view"), logPath)
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id":      "session-123",
		"cwd":             repoRoot,
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"tool_input": map[string]any{
			"command": "npm test",
			"shell":   "/bin/zsh",
		},
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN":             personaStub,
		"CLAUDE_PLUGIN_OPTION_DAEMON_BASE_MODE":        "worktree",
		"CLAUDE_PLUGIN_OPTION_DAEMON_BASE_REF":         "HEAD~1",
		"CLAUDE_PLUGIN_OPTION_DAEMON_ALLOW_DIRTY":      "true",
		"CLAUDE_PLUGIN_OPTION_DAEMON_IGNORED_MODE":     "readonly",
		"CLAUDE_PLUGIN_OPTION_DAEMON_IGNORED_MAX":      "50",
		"CLAUDE_PLUGIN_OPTION_DAEMON_APPLY_MODE":       "reject",
		"PERSONA_TEST_LOG":                             logPath,
	})

	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("unmarshal wrapper response: %v\n%s", err, output)
	}
	command := response["hookSpecificOutput"].(map[string]any)["updatedInput"].(map[string]any)["command"].(string)
	for _, want := range []string{
		"--base-mode worktree",
		"--allow-dirty",
		"--ignored-mode readonly",
		"--ignored-max 50",
		"--apply-mode reject",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("expected wrapped command to include %q, got %q", want, command)
		}
	}
	if !strings.Contains(command, "--base-ref") || !strings.Contains(command, "HEAD~1") {
		t.Fatalf("expected wrapped command to include base-ref override, got %q", command)
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

func TestPersonaWrapFallsBackToCurrentShellWhenToolInputShellIsMissing(t *testing.T) {
	repoRoot := repoRoot(t)
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
			"command": "printf ok",
		},
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub,
		"SHELL":                            "/bin/sh",
	})

	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("unmarshal wrapper response: %v\n%s", err, output)
	}
	command := response["hookSpecificOutput"].(map[string]any)["updatedInput"].(map[string]any)["command"].(string)
	if !strings.Contains(command, "daemon exec --session-key session-123 -- /bin/sh -c 'printf ok'") {
		t.Fatalf("expected wrapper to fall back to current shell with shell-appropriate flags, got %q", command)
	}
}

func TestPersonaWrapNoopsOutsideGitRepositories(t *testing.T) {
	repoRoot := repoRoot(t)
	personaStub := filepath.Join(t.TempDir(), "persona")
	if err := os.WriteFile(personaStub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write persona stub: %v", err)
	}
	nonRepo := t.TempDir()
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id":      "session-123",
		"cwd":             nonRepo,
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"tool_input": map[string]any{
			"command": "printf ok",
		},
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub,
	})
	if len(bytes.TrimSpace(output)) != 0 {
		t.Fatalf("expected non-repo command to bypass wrapping, got %s", output)
	}
}

func TestPersonaWrapRewritesWriteToolsIntoDaemonView(t *testing.T) {
	repoRoot := repoRoot(t)
	viewPath := filepath.Join(t.TempDir(), "view")
	if err := os.MkdirAll(filepath.Join(viewPath, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir view path: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "persona.log")
	personaStub := writePersonaDaemonStub(t, viewPath, logPath)
	originalPath := filepath.Join(repoRoot, "docs", "note.txt")
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id":      "session-123",
		"cwd":             repoRoot,
		"hook_event_name": "PreToolUse",
		"tool_name":       "Write",
		"tool_input": map[string]any{
			"file_path": originalPath,
			"content":   "hello",
		},
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub,
		"PERSONA_TEST_VIEW_PATH":           viewPath,
		"PERSONA_TEST_LOG":                 logPath,
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
	if got, want := updatedInput["content"], "hello"; got != want {
		t.Fatalf("expected content to be preserved: got %v want %q", got, want)
	}
	if got, want := updatedInput["file_path"], filepath.Join(viewPath, "docs", "note.txt"); got != want {
		t.Fatalf("expected file path rewrite into daemon view: got %v want %q", got, want)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read persona stub log: %v", err)
	}
	if !strings.Contains(string(logData), "daemon info --session-key session-123 --json") {
		t.Fatalf("expected daemon info lookup, got log:\n%s", logData)
	}
}

func TestPersonaWrapForwardsDaemonOptionsToSessionLookup(t *testing.T) {
	repoRoot := repoRoot(t)
	viewPath := filepath.Join(t.TempDir(), "view")
	logPath := filepath.Join(t.TempDir(), "persona.log")
	personaStub := writePersonaDaemonStub(t, viewPath, logPath)
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id":      "session-123",
		"cwd":             repoRoot,
		"hook_event_name": "PreToolUse",
		"tool_name":       "Read",
		"tool_input": map[string]any{
			"file_path": filepath.Join(repoRoot, "README.md"),
		},
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN":         personaStub,
		"CLAUDE_PLUGIN_OPTION_DAEMON_BASE_MODE":    "worktree",
		"CLAUDE_PLUGIN_OPTION_DAEMON_BASE_REF":     "HEAD~1",
		"CLAUDE_PLUGIN_OPTION_DAEMON_IGNORED_MODE": "readonly",
		"PERSONA_TEST_VIEW_PATH":                   viewPath,
		"PERSONA_TEST_LOG":                         logPath,
	})

	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("unmarshal wrapper response: %v\n%s", err, output)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read persona stub log: %v", err)
	}
	logText := string(logData)
	for _, want := range []string{"daemon info --session-key session-123", "--base-mode worktree", "--base-ref HEAD~1", "--ignored-mode readonly"} {
		if !strings.Contains(logText, want) {
			t.Fatalf("expected daemon info log to include %q, got log:\n%s", want, logText)
		}
	}
}

func TestPersonaWrapRewritesSearchToolsIntoDaemonView(t *testing.T) {
	repoRoot := repoRoot(t)
	viewPath := filepath.Join(t.TempDir(), "view")
	searchDir := filepath.Join(repoRoot, "internal")
	if err := os.MkdirAll(filepath.Join(viewPath, "internal"), 0o755); err != nil {
		t.Fatalf("mkdir view path: %v", err)
	}
	personaStub := writePersonaDaemonStub(t, viewPath, filepath.Join(t.TempDir(), "persona.log"))
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id":      "session-123",
		"cwd":             filepath.Join(repoRoot, "internal"),
		"hook_event_name": "PreToolUse",
		"tool_name":       "Glob",
		"tool_input": map[string]any{
			"pattern": "*.go",
		},
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub,
		"PERSONA_TEST_VIEW_PATH":           viewPath,
	})

	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("unmarshal wrapper response: %v\n%s", err, output)
	}
	updatedInput := response["hookSpecificOutput"].(map[string]any)["updatedInput"].(map[string]any)
	if got, want := updatedInput["path"], filepath.Join(viewPath, "internal"); got != want {
		t.Fatalf("expected default search path rewrite from cwd %q, got %v want %q", searchDir, got, want)
	}
}

func TestPersonaWrapDeniesGitPathsForFileTools(t *testing.T) {
	repoRoot := repoRoot(t)
	viewPath := filepath.Join(t.TempDir(), "view")
	personaStub := writePersonaDaemonStub(t, viewPath, filepath.Join(t.TempDir(), "persona.log"))
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id":      "session-123",
		"cwd":             repoRoot,
		"hook_event_name": "PreToolUse",
		"tool_name":       "Read",
		"tool_input": map[string]any{
			"file_path": filepath.Join(repoRoot, ".git", "config"),
		},
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub,
		"PERSONA_TEST_VIEW_PATH":           viewPath,
	})

	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("unmarshal wrapper response: %v\n%s", err, output)
	}
	hookOutput := response["hookSpecificOutput"].(map[string]any)
	if got, want := hookOutput["permissionDecision"], "deny"; got != want {
		t.Fatalf("unexpected permission decision: got %v want %q", got, want)
	}
	reason := hookOutput["permissionDecisionReason"].(string)
	if !strings.Contains(reason, ".git") || !strings.Contains(reason, "daemon state") {
		t.Fatalf("unexpected deny reason: %q", reason)
	}
}

func TestPersonaWrapDeniesExternalDaemonPatchPaths(t *testing.T) {
	repoRoot := repoRoot(t)
	viewPath := filepath.Join(t.TempDir(), "view")
	externalGitDir := filepath.Join(t.TempDir(), "external.git")
	patchPath := filepath.Join(externalGitDir, "persona", "daemon", "patches", "session-123.patch")
	personaStub := writePersonaDaemonStub(t, viewPath, filepath.Join(t.TempDir(), "persona.log"))
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id":      "session-123",
		"cwd":             repoRoot,
		"hook_event_name": "PreToolUse",
		"tool_name":       "Read",
		"tool_input": map[string]any{
			"file_path": patchPath,
		},
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub,
		"PERSONA_TEST_VIEW_PATH":           viewPath,
		"PERSONA_TEST_GIT_DIR":             externalGitDir,
		"PERSONA_TEST_PATCH_PATH":          patchPath,
	})

	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("unmarshal wrapper response: %v\n%s", err, output)
	}
	hookOutput := response["hookSpecificOutput"].(map[string]any)
	if got, want := hookOutput["permissionDecision"], "deny"; got != want {
		t.Fatalf("unexpected permission decision: got %v want %q", got, want)
	}
}

func TestPersonaWrapDeniesWritesOutsideRepo(t *testing.T) {
	repoRoot := repoRoot(t)
	viewPath := filepath.Join(t.TempDir(), "view")
	personaStub := writePersonaDaemonStub(t, viewPath, filepath.Join(t.TempDir(), "persona.log"))
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id":      "session-123",
		"cwd":             repoRoot,
		"hook_event_name": "PreToolUse",
		"tool_name":       "Write",
		"tool_input": map[string]any{
			"file_path": filepath.Join(t.TempDir(), "outside.txt"),
			"content":   "hello.txt",
		},
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub,
		"PERSONA_TEST_VIEW_PATH":           viewPath,
	})

	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("unmarshal wrapper response: %v\n%s", err, output)
	}
	hookOutput := response["hookSpecificOutput"].(map[string]any)
	if got, want := hookOutput["permissionDecision"], "deny"; got != want {
		t.Fatalf("unexpected permission decision: got %v want %q", got, want)
	}
	reason := hookOutput["permissionDecisionReason"].(string)
	if !strings.Contains(reason, "only permits direct file writes") || !strings.Contains(reason, "repository") {
		t.Fatalf("unexpected deny reason: %q", reason)
	}
}

func TestPersonaWrapDeniesWritesThroughEscapingViewSymlinks(t *testing.T) {
	repoRoot := repoRoot(t)
	viewPath := filepath.Join(t.TempDir(), "view")
	outside := t.TempDir()
	if err := os.MkdirAll(viewPath, 0o755); err != nil {
		t.Fatalf("mkdir view path: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(viewPath, "escape")); err != nil {
		t.Fatalf("symlink escape dir: %v", err)
	}
	personaStub := writePersonaDaemonStub(t, viewPath, filepath.Join(t.TempDir(), "persona.log"))
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id":      "session-123",
		"cwd":             repoRoot,
		"hook_event_name": "PreToolUse",
		"tool_name":       "Write",
		"tool_input": map[string]any{
			"file_path": filepath.Join(viewPath, "escape", "outside.txt"),
			"content":   "hello",
		},
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub,
		"PERSONA_TEST_VIEW_PATH":           viewPath,
	})

	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("unmarshal wrapper response: %v\n%s", err, output)
	}
	hookOutput := response["hookSpecificOutput"].(map[string]any)
	if got, want := hookOutput["permissionDecision"], "deny"; got != want {
		t.Fatalf("unexpected permission decision: got %v want %q", got, want)
	}
}

func TestPersonaWrapFlushesManagedWriteToolsOnPostToolUse(t *testing.T) {
	repoRoot := repoRoot(t)
	viewPath := filepath.Join(t.TempDir(), "view")
	viewFile := filepath.Join(viewPath, "docs", "note.txt")
	if err := os.MkdirAll(filepath.Dir(viewFile), 0o755); err != nil {
		t.Fatalf("mkdir view dir: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "persona.log")
	personaStub := writePersonaDaemonStub(t, viewPath, logPath)
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id":      "session-123",
		"cwd":             repoRoot,
		"hook_event_name": "PostToolUse",
		"tool_name":       "Write",
		"tool_input": map[string]any{
			"file_path": viewFile,
			"content":   "hello",
		},
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub,
		"PERSONA_TEST_VIEW_PATH":           viewPath,
		"PERSONA_TEST_LOG":                 logPath,
	})

	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("unmarshal post-tool response: %v\n%s", err, output)
	}
	hookOutput := response["hookSpecificOutput"].(map[string]any)
	if got, want := hookOutput["hookEventName"], "PostToolUse"; got != want {
		t.Fatalf("unexpected hook event name: got %v want %q", got, want)
	}
	context, _ := hookOutput["additionalContext"].(string)
	if !strings.Contains(context, "flushed") || !strings.Contains(context, filepath.Join(repoRoot, "docs", "note.txt")) {
		t.Fatalf("expected flush context, got %q", context)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read persona stub log: %v", err)
	}
	if !strings.Contains(string(logData), "daemon flush --session-key session-123") {
		t.Fatalf("expected daemon flush call, got log:\n%s", logData)
	}
}

func TestPersonaWrapAddsPathContextForManagedReadPostToolUse(t *testing.T) {
	repoRoot := repoRoot(t)
	viewPath := filepath.Join(t.TempDir(), "view")
	viewFile := filepath.Join(viewPath, "README.md")
	if err := os.MkdirAll(viewPath, 0o755); err != nil {
		t.Fatalf("mkdir view dir: %v", err)
	}
	personaStub := writePersonaDaemonStub(t, viewPath, filepath.Join(t.TempDir(), "persona.log"))
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id":      "session-123",
		"cwd":             repoRoot,
		"hook_event_name": "PostToolUse",
		"tool_name":       "Read",
		"tool_input": map[string]any{
			"file_path": viewFile,
		},
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub,
		"PERSONA_TEST_VIEW_PATH":           viewPath,
	})

	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("unmarshal post-tool response: %v\n%s", err, output)
	}
	context, _ := response["hookSpecificOutput"].(map[string]any)["additionalContext"].(string)
	if !strings.Contains(context, viewFile) || !strings.Contains(context, filepath.Join(repoRoot, "README.md")) {
		t.Fatalf("expected path mapping context, got %q", context)
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
		"`persona daemon exec --session-key <claude-session-id> -- <selected shell> ...`",
		"lazily starts a per-repo background daemon",
		"./build.sh",
		"./bin/persona doctor",
		"selected shell",
		"`settings.json` sets `\"agent\": \"persona-worker\"`",
		"`DAEMON_BASE_MODE`",
		"`DAEMON_APPLY_MODE`",
		"`Read`, `Edit`, `MultiEdit`, `Write`, `Glob`, and `Grep`",
		"`persona daemon flush --session-key <claude-session-id>`",
		"`persona daemon list --json`",
		"`persona daemon prune --idle-for <duration>`",
		"writes outside the current repository are denied",
		"`.git` and daemon state paths",
		"`--base-mode worktree`",
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
		"`persona daemon exec --session-key <claude-session-id> -- <selected shell> ...`",
		"lazily starts a per-repo daemon",
		"Each Claude chat session key maps to its own daemon-backed patch/view pair.",
		"`persona daemon info --session-key <claude-session-id> --json`",
		"`persona daemon list --json`",
		"`persona daemon flush --session-key <claude-session-id>`",
		"`persona daemon prune --idle-for 24h`",
		"`DAEMON_BASE_MODE`",
		"`DAEMON_ALLOW_DIRTY`",
		"`persona daemon end --session-key <claude-session-id>`",
		"`tool_input.shell` when Claude exposes it",
		"`Read`, `Edit`, `MultiEdit`, `Write`, `Glob`, and `Grep`",
		"`.git` and daemon state paths",
		"`--base-mode worktree`",
		"tool responses may still show internal daemon `view_path` paths",
		"Treat matching paths under",
		"Writes outside the current repository are denied",
		"Commands that resolve to `git`, `gh`, `persona`, or `claude` are bypassed",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("plugin README missing %q:\n%s", want, text)
		}
	}
}

func TestPersonaWorkerAgentDocumentsDaemonWorkflow(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "persona-claude-plugin", "agents", "persona-worker.md"))
	if err != nil {
		t.Fatalf("read persona-worker agent doc: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"`persona daemon exec --session-key <claude-session-id> -- <selected shell> ...`",
		"Repo-scoped `Read`, `Edit`, `MultiEdit`, `Write`, `Glob`, and `Grep`",
		"parallel chats stay isolated",
		"`persona daemon list --json`",
		"`persona daemon prune --idle-for <duration>`",
		"`DAEMON_*`",
		"`persona daemon end --session-key <claude-session-id>`",
		"internal daemon `view_path`",
		"Treat it as repository path",
		"`.git` or daemon state paths",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("persona-worker doc missing %q:\n%s", want, text)
		}
	}
}

func writePersonaDaemonStub(t *testing.T, viewPath, logPath string) string {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "persona-stub")
	script := `#!/bin/sh
set -eu
if [ -n "${PERSONA_TEST_LOG:-}" ]; then
  printf '%s\n' "$*" >>"$PERSONA_TEST_LOG"
fi
if [ "${1:-}" = "daemon" ] && [ "${2:-}" = "info" ]; then
  session=""
  shift 2
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --session-key)
        session="$2"
        shift 2
        ;;
      --json)
        shift
        ;;
      *)
        shift
        ;;
    esac
  done
  git_dir="${PERSONA_TEST_GIT_DIR:-$PWD/.git}"
  patch_path="${PERSONA_TEST_PATCH_PATH:-$git_dir/persona/daemon/patches/$session.patch}"
  printf '{"session_key":"%s","repo_root":"%s","git_dir":"%s","view_path":"%s","patch_path":"%s"}\n' "$session" "$PWD" "$git_dir" "${PERSONA_TEST_VIEW_PATH}" "$patch_path"
  exit 0
fi
if [ "${1:-}" = "daemon" ] && [ "${2:-}" = "flush" ]; then
  exit 0
fi
exit 0
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write persona stub: %v", err)
	}
	if logPath != "" {
		if err := os.WriteFile(logPath, nil, 0o644); err != nil {
			t.Fatalf("init persona log: %v", err)
		}
	}
	if viewPath != "" {
		if err := os.MkdirAll(viewPath, 0o755); err != nil {
			t.Fatalf("mkdir view path: %v", err)
		}
	}
	return scriptPath
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
