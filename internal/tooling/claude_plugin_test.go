package tooling

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
		"FLUSH_MIN_AGE",
		"FLUSH_RETRY_FOR",
		"FLUSH_RETRY_INTERVAL_MS",
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
	for _, event := range []string{"PostToolUseFailure", "PermissionDenied"} {
		entries, ok := hooks[event].([]any)
		if !ok || len(entries) != 1 {
			t.Fatalf("expected lease cleanup hook for %s, got %#v", event, hooks[event])
		}
	}
	sessionEnd, ok := hooks["SessionEnd"].([]any)
	if !ok || len(sessionEnd) != 1 {
		t.Fatalf("expected one SessionEnd handler, got %#v", hooks["SessionEnd"])
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
		if got, want := handler["command"], `"${CLAUDE_PLUGIN_ROOT}/bin/persona-wrap"`; got != want {
			t.Fatalf("unexpected handler command for %q: got %v want %q", matcher, got, want)
		}
		if handler["onFailure"] != "block" {
			t.Fatalf("preparation failures must block original-checkout tools: %#v", handler)
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
	endEntry := sessionEnd[0].(map[string]any)
	endHooks, ok := endEntry["hooks"].([]any)
	if !ok || len(endHooks) != 1 {
		t.Fatalf("unexpected SessionEnd hooks: %#v", endEntry["hooks"])
	}
	endHandler := endHooks[0].(map[string]any)
	if got, want := endHandler["command"], `"${CLAUDE_PLUGIN_ROOT}/bin/persona-wrap"`; got != want {
		t.Fatalf("unexpected SessionEnd handler command: got %v want %q", got, want)
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
	if got, exists := hookOutput["permissionDecision"]; exists {
		t.Fatalf("routing must preserve Claude permission policy, got decision %v: %s", got, output)
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
	if !strings.Contains(command, personaStub+" daemon exec --session-key session-123 --base-mode worktree -- ") {
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
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN":         personaStub,
		"CLAUDE_PLUGIN_OPTION_DAEMON_BASE_MODE":    "worktree",
		"CLAUDE_PLUGIN_OPTION_DAEMON_BASE_REF":     "HEAD~1",
		"CLAUDE_PLUGIN_OPTION_DAEMON_ALLOW_DIRTY":  "true",
		"CLAUDE_PLUGIN_OPTION_DAEMON_IGNORED_MODE": "readonly",
		"CLAUDE_PLUGIN_OPTION_DAEMON_IGNORED_MAX":  "50",
		"CLAUDE_PLUGIN_OPTION_DAEMON_APPLY_MODE":   "reject",
		"PERSONA_TEST_LOG":                         logPath,
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
			"command": "git status --short",
		},
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": "/opt/persona",
		"CLAUDE_PROJECT_DIR":               repoRoot,
	})
	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("unmarshal wrapper response: %v\n%s", err, output)
	}
	hookOutput := response["hookSpecificOutput"].(map[string]any)
	if got, exists := hookOutput["permissionDecision"]; exists {
		t.Fatalf("routing must preserve Claude permission policy, got decision %v: %s", got, output)
	}
	reason := hookOutput["additionalContext"].(string)
	if !strings.Contains(reason, "Git-aware shell commands") || !strings.Contains(reason, ".git") {
		t.Fatalf("unexpected bypass reason: %q", reason)
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
	if !strings.Contains(command, "daemon exec --session-key session-123 --base-mode worktree -- /bin/sh -c 'printf ok'") {
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
	if got, exists := hookOutput["permissionDecision"]; exists {
		t.Fatalf("routing must preserve Claude permission policy, got decision %v: %s", got, output)
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
	if !strings.Contains(string(logData), "daemon acquire --session-key session-123 --base-mode worktree") {
		t.Fatalf("expected daemon lease acquisition, got log:\n%s", logData)
	}
	if !strings.Contains(string(logData), "--owner-pid "+strconv.Itoa(os.Getpid())+" --json") {
		t.Fatalf("expected live owner on daemon lease acquisition, got log:\n%s", logData)
	}
}

func TestPersonaWrapForwardsDaemonOptionsToLeaseAcquisition(t *testing.T) {
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
	for _, want := range []string{"daemon acquire --session-key session-123", "--base-mode worktree", "--base-ref HEAD~1", "--ignored-mode readonly"} {
		if !strings.Contains(logText, want) {
			t.Fatalf("expected daemon acquire log to include %q, got log:\n%s", want, logText)
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
	if !strings.Contains(reason, ".git paths") || !strings.Contains(reason, "git status") {
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
	reason := hookOutput["permissionDecisionReason"].(string)
	if !strings.Contains(reason, "daemon state paths") || !strings.Contains(reason, "persona daemon info") {
		t.Fatalf("unexpected daemon-state deny reason: %q", reason)
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
	if !strings.Contains(string(logData), "daemon release --session-key session-123 --lease-id lease-123 --view-generation 7") {
		t.Fatalf("expected generation-aware daemon release, got log:\n%s", logData)
	}
}

func TestPersonaWrapAlwaysSavesLeasesDespiteLegacyFlushMinAge(t *testing.T) {
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
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN":   personaStub,
		"CLAUDE_PLUGIN_OPTION_FLUSH_MIN_AGE": "1s",
		"PERSONA_TEST_VIEW_PATH":             viewPath,
		"PERSONA_TEST_LOG":                   logPath,
	})

	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("unmarshal post-tool response: %v\n%s", err, output)
	}
	context, _ := response["hookSpecificOutput"].(map[string]any)["additionalContext"].(string)
	if !strings.Contains(context, "durably flushed") || strings.Contains(context, "deferred") {
		t.Fatalf("expected min-age context, got %q", context)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read persona stub log: %v", err)
	}
	if strings.Contains(string(logData), "--min-age") || !strings.Contains(string(logData), "daemon release --session-key session-123") {
		t.Fatalf("expected durable release without min-age skipping, got log:\n%s", logData)
	}
}

func TestPersonaWrapRetainsLeaseAndReportsFailedSave(t *testing.T) {
	repoRoot := repoRoot(t)
	viewPath := filepath.Join(t.TempDir(), "view")
	logPath := filepath.Join(t.TempDir(), "persona.log")
	failCount := filepath.Join(t.TempDir(), "release-failures")
	if err := os.WriteFile(failCount, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	leaseDir := t.TempDir()
	personaStub := writePersonaDaemonStub(t, viewPath, logPath)
	event := map[string]any{
		"session_id": "session-123", "cwd": repoRoot, "hook_event_name": "PostToolUse",
		"tool_name": "Write", "tool_input": map[string]any{"file_path": filepath.Join(viewPath, "note.txt"), "content": "hello"},
	}
	env := map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub, "PERSONA_TEST_VIEW_PATH": viewPath,
		"PERSONA_TEST_LOG": logPath, "PERSONA_TEST_RELEASE_FAIL_COUNT_FILE": failCount,
		"PERSONA_WRAP_LEASE_DIR": leaseDir,
	}
	output := runPersonaWrap(t, repoRoot, event, env)
	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("decode failure context: %v\n%s", err, output)
	}
	context := response["hookSpecificOutput"].(map[string]any)["additionalContext"].(string)
	if !strings.Contains(context, "NOT confirmed") || !strings.Contains(context, "Do not end, prune") || response["systemMessage"] != context {
		t.Fatalf("save failure must be visible to Claude and user: %s", output)
	}
	entries, err := personaLeaseRecordEntries(leaseDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("failed release must retain lease record: entries=%v err=%v", entries, err)
	}
	env["PERSONA_TEST_SKIP_PRETOOL"] = "1"
	output = runPersonaWrap(t, repoRoot, event, env)
	if !bytes.Contains(output, []byte("durably flushed")) {
		t.Fatalf("retry did not save retained lease: %s", output)
	}
	entries, err = personaLeaseRecordEntries(leaseDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("successful release must remove lease record: entries=%v err=%v", entries, err)
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

func TestPersonaWrapEndsDaemonSessionOnSessionEnd(t *testing.T) {
	repoRoot := repoRoot(t)
	viewPath := filepath.Join(t.TempDir(), "view")
	logPath := filepath.Join(t.TempDir(), "persona.log")
	personaStub := writePersonaDaemonStub(t, viewPath, logPath)

	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id":      "session-123",
		"cwd":             repoRoot,
		"hook_event_name": "SessionEnd",
		"reason":          "exit",
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub,
		"PERSONA_TEST_VIEW_PATH":           viewPath,
		"PERSONA_TEST_LOG":                 logPath,
	})

	if len(bytes.TrimSpace(output)) != 0 {
		t.Fatalf("expected SessionEnd hook to stay silent, got %s", output)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read persona stub log: %v", err)
	}
	if !strings.Contains(string(logData), "daemon end --session-key session-123") {
		t.Fatalf("expected daemon end call on SessionEnd, got log:\n%s", logData)
	}
}

func TestPersonaWrapEndsEveryRegisteredRepositoryFromOutsideGit(t *testing.T) {
	repoRoot := repoRoot(t)
	view, leaseDir, logDir := t.TempDir(), t.TempDir(), t.TempDir()
	personaStub := writePersonaDaemonStub(t, view, "")
	logPath := filepath.Join(logDir, "cwd.log")
	env := map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub, "PERSONA_TEST_VIEW_PATH": view,
		"PERSONA_WRAP_LEASE_DIR": leaseDir, "PERSONA_TEST_CWD_LOG": logPath,
	}
	projects := []string{newPersonaHookRepository(t), newPersonaHookRepository(t)}
	for _, project := range projects {
		output := runPersonaWrap(t, repoRoot, map[string]any{
			"session_id": "multi-repo", "cwd": project, "hook_event_name": "PreToolUse", "tool_name": "Bash",
			"tool_input": map[string]any{"command": "printf changed > note.txt"},
		}, env)
		if !bytes.Contains(output, []byte("updatedInput")) {
			t.Fatalf("register Bash repository: %s", output)
		}
	}
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id": "other-session", "cwd": projects[0], "hook_event_name": "PreToolUse", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "printf other"},
	}, env)
	if !bytes.Contains(output, []byte("updatedInput")) {
		t.Fatalf("register other session: %s", output)
	}
	output = runPersonaWrap(t, repoRoot, map[string]any{
		"session_id": "multi-repo", "cwd": t.TempDir(), "hook_event_name": "SessionEnd",
	}, env)
	if len(bytes.TrimSpace(output)) != 0 {
		t.Fatalf("end registered repositories: %s", output)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range projects {
		if !bytes.Contains(logData, []byte(project+" daemon end --session-key multi-repo")) {
			t.Fatalf("repository %s not ended: %s", project, logData)
		}
	}
	if bytes.Contains(logData, []byte("other-session")) {
		t.Fatalf("SessionEnd touched another session: %s", logData)
	}
	entries, err := os.ReadDir(leaseDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("only the other session registration should remain: %v %v", entries, err)
	}
}

func TestPersonaWrapSessionEndIsolatesFailuresAndRetainsRetryState(t *testing.T) {
	repoRoot := repoRoot(t)
	for _, failure := range []string{"daemon", "timeout", "corrupt-registration"} {
		t.Run(failure, func(t *testing.T) {
			view, leaseDir := t.TempDir(), t.TempDir()
			personaStub := writePersonaDaemonStub(t, view, "")
			logPath := filepath.Join(t.TempDir(), "cwd.log")
			broken, healthy := newPersonaHookRepository(t), newPersonaHookRepository(t)
			env := map[string]string{
				"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub, "PERSONA_TEST_VIEW_PATH": view,
				"PERSONA_WRAP_LEASE_DIR": leaseDir, "PERSONA_TEST_CWD_LOG": logPath,
				"PERSONA_TEST_END_FAIL_REPO": broken,
			}
			for _, project := range []string{broken, healthy} {
				output := runPersonaWrap(t, repoRoot, map[string]any{
					"session_id": "multi-repo", "cwd": project, "hook_event_name": "PreToolUse", "tool_name": "Bash",
					"tool_input": map[string]any{"command": "printf ok"},
				}, env)
				if !bytes.Contains(output, []byte("updatedInput")) {
					t.Fatalf("register repository: %s", output)
				}
			}
			var brokenPath string
			if failure == "corrupt-registration" {
				entries, err := os.ReadDir(leaseDir)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					path := filepath.Join(leaseDir, entry.Name())
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Contains(data, []byte(broken)) {
						brokenPath = path
						if err := os.WriteFile(path, []byte("{interrupted"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				if brokenPath == "" {
					t.Fatal("missing broken repository registration")
				}
			}
			if failure == "timeout" {
				env["PERSONA_WRAP_DAEMON_TIMEOUT"] = "500ms"
				env["PERSONA_TEST_END_SLEEP_REPO"] = broken
			}
			event := map[string]any{"session_id": "multi-repo", "cwd": t.TempDir(), "hook_event_name": "SessionEnd"}
			started := time.Now()
			output := runPersonaWrap(t, repoRoot, event, env)
			if failure == "timeout" && time.Since(started) > 2*time.Second {
				t.Fatal("SessionEnd exceeded its shared timeout budget")
			}
			if !bytes.Contains(output, []byte("failed during SessionEnd")) {
				t.Fatalf("failure must remain visible: %s", output)
			}
			logData, err := os.ReadFile(logPath)
			if err != nil || !bytes.Contains(logData, []byte(healthy+" daemon end --session-key multi-repo")) {
				t.Fatalf("failed repository blocked healthy cleanup: %s %v", logData, err)
			}
			entries, err := os.ReadDir(leaseDir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("only failed registration must remain: %v %v", entries, err)
			}
			if failure == "corrupt-registration" {
				if !bytes.Contains(output, []byte(brokenPath)) {
					t.Fatalf("corrupt registration must report recovery path: %s", output)
				}
				return
			}
			env["PERSONA_TEST_END_FAIL_REPO"] = ""
			env["PERSONA_TEST_END_SLEEP_REPO"] = ""
			if output := runPersonaWrap(t, repoRoot, event, env); len(bytes.TrimSpace(output)) != 0 {
				t.Fatalf("retry failed: %s", output)
			}
			entries, err = os.ReadDir(leaseDir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("successful retry must remove final registration: %v %v", entries, err)
			}
		})
	}
}

func TestPersonaWrapPostToolUsesAcquiredRepositoryAfterCwdChanges(t *testing.T) {
	repoRoot := repoRoot(t)
	view, project := t.TempDir(), newPersonaHookRepository(t)
	personaStub := writePersonaDaemonStub(t, view, "")
	logPath := filepath.Join(t.TempDir(), "cwd.log")
	env := map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub, "PERSONA_TEST_VIEW_PATH": view,
		"PERSONA_WRAP_LEASE_DIR": t.TempDir(), "PERSONA_TEST_CWD_LOG": logPath,
		"PERSONA_TEST_SKIP_PRETOOL": "1",
	}
	event := map[string]any{
		"session_id": "moved-cwd", "tool_use_id": "write-1", "cwd": project,
		"hook_event_name": "PreToolUse", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": "note.txt", "content": "saved"},
	}
	if output := runPersonaWrap(t, repoRoot, event, env); !bytes.Contains(output, []byte("updatedInput")) {
		t.Fatalf("acquire lease: %s", output)
	}
	event["hook_event_name"], event["cwd"] = "PostToolUse", t.TempDir()
	env["CLAUDE_PLUGIN_OPTION_PERSONA_BIN"] = filepath.Join(t.TempDir(), "missing-persona")
	if output := runPersonaWrap(t, repoRoot, event, env); !bytes.Contains(output, []byte("durably flushed")) {
		t.Fatalf("post hook must use original repository and binary: %s", output)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil || !bytes.Contains(logData, []byte(project+" daemon release --session-key moved-cwd --lease-id lease-123 --view-generation 7")) {
		t.Fatalf("release lost acquired repository identity: %s %v", logData, err)
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
		"./build.sh",
		"./bin/persona doctor",
		"selected shell",
		"`settings.json` sets `\"agent\": \"persona-worker\"`",
		"`DAEMON_BASE_MODE`",
		"`DAEMON_APPLY_MODE`",
		"`Read`, `Edit`, `MultiEdit`, `Write`, `Glob`, and `Grep`",
		"`persona daemon list --json`",
		"`persona daemon acquire`",
		"`persona daemon release`",
		"`view_generation`",
		"`PostToolUseFailure`",
		"`PermissionDenied`",
		"`permissionDecision: allow`",
		"single literal",
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
		"`FLUSH_MIN_AGE`",
		"`FLUSH_RETRY_FOR`",
		"`persona daemon end --session-key <claude-session-id>`",
		"`SessionEnd` runs `persona daemon end --session-key <claude-session-id>`",
		"`tool_input.shell` when Claude exposes it",
		"`Read`, `Edit`, `MultiEdit`, `Write`, `Glob`, and `Grep`",
		"deny reason tells Claude whether it should switch to Git-aware commands or `persona daemon` commands",
		"`--base-mode worktree`",
		"tool responses may still show internal daemon `view_path` paths",
		"Treat matching paths under",
		"Writes outside the current repository are denied",
		"Single literal commands for `git`, `gh`, `persona`, or `claude` may bypass",
		"acquire a mutation lease before the tool runs",
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
		"`FLUSH_MIN_AGE`",
		"`persona daemon end --session-key <claude-session-id>`",
		"plugin runs `persona daemon end --session-key <claude-session-id>` automatically",
		"internal daemon `view_path`",
		"Treat it as repository path",
		"deny reason tells you whether to switch to Git-aware tooling or explicit `persona daemon` commands",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("persona-worker doc missing %q:\n%s", want, text)
		}
	}
}

func TestPersonaWrapRejectsCompoundBypassWithoutTouchingOriginal(t *testing.T) {
	repoRoot := repoRoot(t)
	project := t.TempDir()
	if output, err := exec.Command("git", "-C", project, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("init test repo: %v: %s", err, output)
	}
	sentinel := filepath.Join(project, "tracked.txt")
	if err := os.WriteFile(sentinel, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"git status && printf changed > tracked.txt",
		"git&&printf changed > tracked.txt",
		"git>tracked.txt",
		"git status > tracked.txt",
		"git status; python3 edit.py",
		"git status | tee tracked.txt",
		"git status & printf changed > tracked.txt",
		"git status $(printf changed > tracked.txt)",
		"git status `printf changed > tracked.txt`",
		"git show -- *.txt",
		"git status <<EOF\nhello\nEOF",
		"git status\nprintf changed > tracked.txt",
		`git status "$(printf changed > tracked.txt)"`,
		"gh repo view; printf changed > tracked.txt",
		"persona doctor > tracked.txt",
		"claude --version && printf changed > tracked.txt",
	} {
		t.Run(command, func(t *testing.T) {
			output := runPersonaWrap(t, repoRoot, map[string]any{
				"session_id": "session-123", "cwd": project, "hook_event_name": "PreToolUse",
				"tool_name": "Bash", "tool_input": map[string]any{"command": command},
			}, nil)
			var response map[string]any
			if err := json.Unmarshal(output, &response); err != nil {
				t.Fatalf("decode decision: %v: %s", err, output)
			}
			hook := response["hookSpecificOutput"].(map[string]any)
			if hook["permissionDecision"] != "deny" || hook["updatedInput"] != nil {
				t.Fatalf("compound bypass must be denied: %s", output)
			}
			if !strings.Contains(hook["permissionDecisionReason"].(string), "separately") {
				t.Fatalf("denial must explain how to split the command: %s", output)
			}
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "original" {
				t.Fatalf("original sentinel changed: %q %v", data, err)
			}
		})
	}
}

func TestPersonaWrapAllowsQuotedLiteralBypassWithoutApprovingIt(t *testing.T) {
	repoRoot := repoRoot(t)
	for _, command := range []string{
		"git status --short",
		`git log --format='literal && ; | > $(text)'`,
		`git log --format="literal && ; >"`,
		`git show -- 'semi;colon.txt'`,
		`git show -- semi\;colon.txt`,
	} {
		t.Run(command, func(t *testing.T) {
			output := runPersonaWrap(t, repoRoot, map[string]any{
				"session_id": "session-123", "cwd": repoRoot, "hook_event_name": "PreToolUse",
				"tool_name": "Bash", "tool_input": map[string]any{"command": command},
			}, nil)
			var response map[string]any
			if err := json.Unmarshal(output, &response); err != nil {
				t.Fatalf("decode bypass: %v: %s", err, output)
			}
			hook := response["hookSpecificOutput"].(map[string]any)
			if hook["permissionDecision"] != nil || hook["updatedInput"] != nil || hook["additionalContext"] == nil {
				t.Fatalf("literal bypass must preserve normal permission checks: %s", output)
			}
		})
	}
}

func TestPersonaWrapKeepsShellWrappersInsideView(t *testing.T) {
	repoRoot := repoRoot(t)
	view := t.TempDir()
	personaStub := writePersonaDaemonStub(t, view, "")
	for _, command := range []string{
		"env git status; printf changed > tracked.txt",
		"env -S 'git status'; printf changed > tracked.txt",
		"sudo -u nobody git status; printf changed > tracked.txt",
		"bash -c 'git status; printf changed > tracked.txt'",
		"sh -c 'git status; printf changed > tracked.txt'",
		"command git status; printf changed > tracked.txt",
		"GIT_OPTIONAL_LOCKS=0 git status; printf changed > tracked.txt",
	} {
		t.Run(command, func(t *testing.T) {
			output := runPersonaWrap(t, repoRoot, map[string]any{
				"session_id": "session-123", "cwd": repoRoot, "hook_event_name": "PreToolUse",
				"tool_name": "Bash", "tool_input": map[string]any{"command": command, "shell": "/bin/sh"},
			}, map[string]string{"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub})
			var response map[string]any
			if err := json.Unmarshal(output, &response); err != nil {
				t.Fatalf("decode routing: %v: %s", err, output)
			}
			hook := response["hookSpecificOutput"].(map[string]any)
			updated, ok := hook["updatedInput"].(map[string]any)
			if !ok || hook["permissionDecision"] != nil || !strings.Contains(updated["command"].(string), " daemon exec ") {
				t.Fatalf("wrapped shell must stay inside persona: %s", output)
			}
			// Execute representative wrappers through a CWD-isolating stub to
			// verify the complete shell payload, not only token classification.
			if strings.HasPrefix(command, "env git") || strings.HasPrefix(command, "bash -c") {
				original := filepath.Join(t.TempDir(), "tracked.txt")
				if err := os.WriteFile(original, []byte("original"), 0o600); err != nil {
					t.Fatal(err)
				}
				run := exec.Command("/bin/sh", "-c", updated["command"].(string))
				run.Dir = filepath.Dir(original)
				run.Env = append(os.Environ(), "PERSONA_TEST_VIEW_PATH="+view)
				if data, err := run.CombinedOutput(); err != nil {
					t.Fatalf("execute rewritten command: %v: %s", err, data)
				}
				data, err := os.ReadFile(original)
				if err != nil || string(data) != "original" {
					t.Fatalf("wrapper changed original checkout: %q %v", data, err)
				}
				data, err = os.ReadFile(filepath.Join(view, "tracked.txt"))
				if err != nil || string(data) != "changed" {
					t.Fatalf("wrapper did not execute mutation inside view: %q %v", data, err)
				}
			}
		})
	}
}

func TestPersonaWrapConfiguredRegexCannotBypassCompoundCommands(t *testing.T) {
	repoRoot := repoRoot(t)
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id": "session-123", "cwd": repoRoot, "hook_event_name": "PreToolUse", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "printf changed > tracked.txt"},
	}, map[string]string{"PERSONA_WRAP_BYPASS_REGEX": ".*"})
	if !bytes.Contains(output, []byte(`"permissionDecision": "deny"`)) {
		t.Fatalf("configured regex must not override shell safety: %s", output)
	}
}

func TestPersonaWrapRechecksTranslatedSymlinksAndProtectedAliases(t *testing.T) {
	repoRoot := repoRoot(t)
	for _, scenario := range []string{"view-only", "intermediate", "dangling", "git-alias", "daemon-alias", "default-search"} {
		t.Run(scenario, func(t *testing.T) {
			view := filepath.Join(t.TempDir(), "view")
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "sentinel.txt")
			if err := os.WriteFile(sentinel, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(t.TempDir(), "daemon.log")
			personaStub := writePersonaDaemonStub(t, view, logPath)
			name := "persona-review-view-only"
			target := outside
			if scenario == "intermediate" {
				name += "/middle"
				if err := os.MkdirAll(filepath.Dir(filepath.Join(view, name)), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "dangling" {
				target = filepath.Join(outside, "missing")
			}
			if scenario == "git-alias" {
				target = filepath.Join(view, ".git")
			}
			if scenario == "daemon-alias" {
				target = filepath.Join(repoRoot, ".git", "persona", "daemon")
			}
			if err := os.Symlink(target, filepath.Join(view, name)); err != nil {
				t.Fatal(err)
			}
			tool := "Write"
			input := map[string]any{"file_path": filepath.Join(repoRoot, name, "sentinel.txt"), "content": "changed"}
			if scenario == "default-search" {
				tool = "Glob"
				input = map[string]any{"path": filepath.Join(repoRoot, name), "pattern": "*"}
			}
			output := runPersonaWrap(t, repoRoot, map[string]any{
				"session_id": "session-123", "cwd": repoRoot, "hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": input,
			}, map[string]string{"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub, "PERSONA_TEST_VIEW_PATH": view, "PERSONA_TEST_LOG": logPath})
			if !bytes.Contains(output, []byte(`"permissionDecision": "deny"`)) {
				t.Fatalf("translated escaping/protected target must be denied: %s", output)
			}
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "original" {
				t.Fatalf("outside sentinel changed: %q %v", data, err)
			}
			logData, err := os.ReadFile(logPath)
			if err != nil || !bytes.Contains(logData, []byte("daemon release")) {
				t.Fatalf("denied target must release acquired lease: %s %v", logData, err)
			}
		})
	}
}

func TestPersonaWrapAllowsManagedViewInsideDaemonDirectory(t *testing.T) {
	repoRoot := repoRoot(t)
	project := t.TempDir()
	if output, err := exec.Command("git", "-C", project, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("init repo: %v %s", err, output)
	}
	view := filepath.Join(project, ".git", "persona", "daemon", "sessions", "session-123", "view")
	personaStub := writePersonaDaemonStub(t, view, "")
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id": "session-123", "cwd": project, "hook_event_name": "PreToolUse", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": filepath.Join(project, "note.txt"), "content": "hello"},
	}, map[string]string{"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub, "PERSONA_TEST_VIEW_PATH": view})
	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatal(err)
	}
	hook := response["hookSpecificOutput"].(map[string]any)
	if hook["permissionDecision"] != nil || hook["updatedInput"] == nil {
		t.Fatalf("managed view itself is not daemon metadata: %s", output)
	}
}

func TestPersonaWrapRejectsMissingSessionOrToolIdentity(t *testing.T) {
	repoRoot := repoRoot(t)
	for _, tool := range []string{"Bash", "Write", "Read"} {
		t.Run(tool, func(t *testing.T) {
			output := runPersonaWrap(t, repoRoot, map[string]any{
				"cwd": repoRoot, "hook_event_name": "PreToolUse", "tool_name": tool,
				"tool_input": map[string]any{"command": "printf ok", "file_path": filepath.Join(repoRoot, "README.md")},
			}, nil)
			if !bytes.Contains(output, []byte(`"permissionDecision": "deny"`)) || !bytes.Contains(output, []byte("session_id")) {
				t.Fatalf("missing identity must not share a session: %s", output)
			}
		})
	}
	view := t.TempDir()
	personaStub := writePersonaDaemonStub(t, view, "")
	output := runPersonaWrapEvent(t, repoRoot, map[string]any{
		"session_id": "session-123", "cwd": repoRoot, "hook_event_name": "PreToolUse", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": filepath.Join(repoRoot, "note.txt"), "content": "hello"},
	}, map[string]string{"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub})
	if !bytes.Contains(output, []byte(`"permissionDecision": "deny"`)) || !bytes.Contains(output, []byte("tool_use_id")) {
		t.Fatalf("missing tool identity must be denied: %s", output)
	}
}

func TestPersonaWrapRepoDiscoveryIgnoresGitEnvironment(t *testing.T) {
	repoRoot := repoRoot(t)
	personaStub := writePersonaDaemonStub(t, t.TempDir(), "")
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id": "session-123", "cwd": repoRoot, "hook_event_name": "PreToolUse", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "printf ok"},
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub, "GIT_DIR": filepath.Join(t.TempDir(), "not-a-repo"),
		"GIT_WORK_TREE": t.TempDir(), "GIT_INDEX_FILE": filepath.Join(t.TempDir(), "index"),
	})
	if !bytes.Contains(output, []byte("daemon exec --session-key session-123")) {
		t.Fatalf("inherited Git environment must not change repo discovery: %s", output)
	}
}

func TestPersonaWrapDoesNotTreatGitLookupFailureAsOutsideRepo(t *testing.T) {
	repoRoot := repoRoot(t)
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id": "session-123", "cwd": repoRoot, "hook_event_name": "PreToolUse", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "printf changed > tracked.txt"},
	}, map[string]string{"PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH")})
	if !bytes.Contains(output, []byte(`"permissionDecision": "deny"`)) {
		t.Fatalf("failed repository discovery must not run the original command: %s", output)
	}
}

func TestPersonaWrapMalformedInputReturnsBlockingExit(t *testing.T) {
	repoRoot := repoRoot(t)
	command := exec.Command(filepath.Join(repoRoot, "persona-claude-plugin", "bin", "persona-wrap"))
	command.Stdin = strings.NewReader("{incomplete")
	output, err := command.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 || !bytes.Contains(output, []byte("persona hook input failed")) {
		t.Fatalf("malformed hook input must return blocking exit 2: %v %s", err, output)
	}
}

func TestPersonaWrapBoundsDaemonSubprocessTime(t *testing.T) {
	repoRoot := repoRoot(t)
	view := t.TempDir()
	personaStub := writePersonaDaemonStub(t, view, "")
	start := time.Now()
	output := runPersonaWrap(t, repoRoot, map[string]any{
		"session_id": "session-123", "cwd": repoRoot, "hook_event_name": "PreToolUse", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": filepath.Join(repoRoot, "note.txt"), "content": "hello"},
	}, map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub, "PERSONA_TEST_VIEW_PATH": view,
		"PERSONA_TEST_SLEEP": "3", "PERSONA_WRAP_DAEMON_TIMEOUT": "50ms",
	})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("daemon timeout did not bound hook: %s", elapsed)
	}
	if !bytes.Contains(output, []byte(`"permissionDecision": "deny"`)) || !bytes.Contains(output, []byte("timed out")) {
		t.Fatalf("timeout must deny the tool with context: %s", output)
	}
}

func TestPersonaWrapFileToolsShareLeaseAndReleaseAfterFailureOrDenial(t *testing.T) {
	repoRoot := repoRoot(t)
	for _, outcome := range []string{"PostToolUseFailure", "PermissionDenied"} {
		t.Run(outcome, func(t *testing.T) {
			view := t.TempDir()
			leaseDir := t.TempDir()
			active := filepath.Join(t.TempDir(), "active")
			logPath := filepath.Join(t.TempDir(), "daemon.log")
			personaStub := writePersonaDaemonStub(t, view, logPath)
			env := map[string]string{
				"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub, "PERSONA_TEST_VIEW_PATH": view,
				"PERSONA_WRAP_LEASE_DIR": leaseDir, "PERSONA_TEST_ACTIVE_LEASE_FILE": active,
				"PERSONA_TEST_LOG": logPath, "PERSONA_TEST_SKIP_PRETOOL": "1",
			}
			event := map[string]any{
				"session_id": "session-123", "tool_use_id": "write-1", "cwd": repoRoot,
				"hook_event_name": "PreToolUse", "tool_name": "Write",
				"tool_input": map[string]any{"file_path": filepath.Join(repoRoot, "note.txt"), "content": "hello"},
			}
			output := runPersonaWrap(t, repoRoot, event, env)
			if !bytes.Contains(output, []byte("updatedInput")) {
				t.Fatalf("first writer did not acquire: %s", output)
			}
			entries, err := personaLeaseRecordEntries(leaseDir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("missing private lease record: %v %v", entries, err)
			}
			info, err := entries[0].Info()
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("lease record permissions: %v %v", info, err)
			}
			read := map[string]any{
				"session_id": "session-123", "tool_use_id": "read-2", "cwd": repoRoot,
				"hook_event_name": "PreToolUse", "tool_name": "Read",
				"tool_input": map[string]any{"file_path": filepath.Join(repoRoot, "note.txt")},
			}
			output = runPersonaWrap(t, repoRoot, read, env)
			if !bytes.Contains(output, []byte(`"permissionDecision": "deny"`)) || !bytes.Contains(output, []byte("still busy")) {
				t.Fatalf("read must not race an active mutation: %s", output)
			}
			event["hook_event_name"] = outcome
			output = runPersonaWrap(t, repoRoot, event, env)
			if outcome == "PostToolUseFailure" && !bytes.Contains(output, []byte("partial file changes")) {
				t.Fatalf("failure must flush partial state: %s", output)
			}
			entries, err = personaLeaseRecordEntries(leaseDir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("completion must release private record: %v %v", entries, err)
			}
			output = runPersonaWrap(t, repoRoot, read, env)
			if !bytes.Contains(output, []byte("updatedInput")) {
				t.Fatalf("next reader did not acquire after release: %s", output)
			}
			logData, err := os.ReadFile(logPath)
			if err != nil || !bytes.Contains(logData, []byte("--lease-id lease-123 --view-generation 7")) {
				t.Fatalf("release lost lease identity: %s %v", logData, err)
			}
		})
	}
}

func TestPersonaWrapDoesNotFlushReplacementGeneration(t *testing.T) {
	repoRoot := repoRoot(t)
	view := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	personaStub := writePersonaDaemonStub(t, view, logPath)
	env := map[string]string{
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN": personaStub, "PERSONA_TEST_VIEW_PATH": view,
		"PERSONA_WRAP_LEASE_DIR": t.TempDir(), "PERSONA_TEST_LOG": logPath, "PERSONA_TEST_SKIP_PRETOOL": "1",
	}
	event := map[string]any{
		"session_id": "session-123", "tool_use_id": "write-1", "cwd": repoRoot,
		"hook_event_name": "PreToolUse", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": filepath.Join(repoRoot, "note.txt"), "content": "hello"},
	}
	if output := runPersonaWrap(t, repoRoot, event, env); !bytes.Contains(output, []byte("updatedInput")) {
		t.Fatalf("acquire failed: %s", output)
	}
	env["PERSONA_TEST_VIEW_GENERATION"] = "8"
	event["hook_event_name"] = "PostToolUse"
	output := runPersonaWrap(t, repoRoot, event, env)
	if !bytes.Contains(output, []byte("NOT confirmed")) || !bytes.Contains(output, []byte("stale view generation")) {
		t.Fatalf("restart must preserve uncertainty: %s", output)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil || bytes.Contains(logData, []byte("daemon info")) || !bytes.Contains(logData, []byte("--view-generation 7")) {
		t.Fatalf("post hook must only use acquired generation: %s %v", logData, err)
	}
}

func TestPersonaClaudeHookCommandWorksWithSpacesInPluginPath(t *testing.T) {
	repoRoot := repoRoot(t)
	pluginRoot := filepath.Join(t.TempDir(), "plugin with spaces")
	if err := os.MkdirAll(filepath.Join(pluginRoot, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "persona-claude-plugin", "bin", "persona-wrap"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, "bin", "persona-wrap"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(repoRoot, "persona-claude-plugin", "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	command := config.Hooks["PreToolUse"][0].Hooks[0].Command
	run := exec.Command("/bin/sh", "-c", command)
	run.Dir = repoRoot
	run.Env = append(os.Environ(), "CLAUDE_PLUGIN_ROOT="+pluginRoot)
	input, err := json.Marshal(map[string]any{
		"session_id": "session-123", "cwd": repoRoot, "hook_event_name": "PreToolUse", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "git status --short"},
	})
	if err != nil {
		t.Fatal(err)
	}
	run.Stdin = bytes.NewReader(input)
	output, err := run.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("Git-aware shell commands")) {
		t.Fatalf("quoted hook path did not execute: %v %s", err, output)
	}
}

func TestPersonaWrapFindsLiveClaudeAncestorInsteadOfHookPID(t *testing.T) {
	repoRoot := repoRoot(t)
	view := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	personaStub := writePersonaDaemonStub(t, view, logPath)
	leaseDir := t.TempDir()
	if err := os.Chmod(leaseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	launcher := `import json,os,subprocess,sys
result=subprocess.run([sys.argv[1]],input=sys.stdin.buffer.read(),capture_output=True)
print(json.dumps({"owner_pid":os.getpid(),"stdout":result.stdout.decode(),"stderr":result.stderr.decode(),"exit":result.returncode}))
`
	command := exec.Command("python3", "-c", launcher, filepath.Join(repoRoot, "persona-claude-plugin", "bin", "persona-wrap"))
	command.Args[0] = "claude"
	command.Dir = repoRoot
	command.Env = append(os.Environ(), "PERSONA_WRAP_OWNER_PID=", "PERSONA_WRAP_LEASE_DIR="+leaseDir,
		"CLAUDE_PLUGIN_OPTION_PERSONA_BIN="+personaStub, "PERSONA_TEST_VIEW_PATH="+view, "PERSONA_TEST_LOG="+logPath)
	input, err := json.Marshal(map[string]any{
		"session_id": "session-123", "tool_use_id": "write-1", "cwd": repoRoot,
		"hook_event_name": "PreToolUse", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": filepath.Join(repoRoot, "note.txt"), "content": "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run launcher: %v %s", err, output)
	}
	var result struct {
		OwnerPID int    `json:"owner_pid"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		Exit     int    `json:"exit"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	if result.Exit != 0 || !strings.Contains(result.Stdout, "updatedInput") {
		t.Fatalf("ancestor-owned acquire failed: %#v", result)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil || !bytes.Contains(logData, []byte("--owner-pid "+strconv.Itoa(result.OwnerPID))) {
		t.Fatalf("acquire did not use live Claude ancestor: %s %v", logData, err)
	}
}

func writePersonaDaemonStub(t *testing.T, viewPath, logPath string) string {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "persona-stub")
	script := `#!/usr/bin/env python3
import json
import os
import subprocess
import sys
import time
args = sys.argv[1:]
if os.getenv("PERSONA_TEST_LOG"):
    with open(os.environ["PERSONA_TEST_LOG"], "a") as log:
        log.write(" ".join(args) + "\n")
if os.getenv("PERSONA_TEST_CWD_LOG"):
    with open(os.environ["PERSONA_TEST_CWD_LOG"], "a") as log:
        log.write(os.getcwd() + " " + " ".join(args) + "\n")
if os.getenv("PERSONA_TEST_SLEEP"):
    time.sleep(float(os.environ["PERSONA_TEST_SLEEP"]))
def option(name, default=""):
    return args[args.index(name) + 1] if name in args else default
method = args[1] if len(args) > 1 else ""
if method == "end" and os.getenv("PERSONA_TEST_END_SLEEP_REPO") == os.getcwd():
    time.sleep(3)
session = option("--session-key")
git_dir = os.getenv("PERSONA_TEST_GIT_DIR", os.path.join(os.getcwd(), ".git"))
info = {"session_key": session, "repo_root": os.getcwd(), "git_dir": git_dir,
        "view_path": os.environ["PERSONA_TEST_VIEW_PATH"],
        "patch_path": os.getenv("PERSONA_TEST_PATCH_PATH", os.path.join(git_dir, "persona", "daemon", "patches", session + ".patch")),
        "view_generation": int(os.getenv("PERSONA_TEST_VIEW_GENERATION", "7"))}
active_path = os.getenv("PERSONA_TEST_ACTIVE_LEASE_FILE")
if method == "acquire":
    os.kill(int(option("--owner-pid")), 0)
    if active_path:
        try:
            fd = os.open(active_path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            os.close(fd)
        except FileExistsError:
            sys.stderr.write("session is still busy\n")
            sys.exit(1)
    print(json.dumps({"session": info, "lease_id": "lease-123"}))
elif method == "info":
    print(json.dumps(info))
elif method == "release":
    if int(option("--view-generation", "0")) != info["view_generation"]:
        sys.stderr.write("stale view generation\n")
        sys.exit(1)
    fail_count = os.getenv("PERSONA_TEST_RELEASE_FAIL_COUNT_FILE")
    if fail_count and os.path.exists(fail_count):
        with open(fail_count) as stream:
            remaining = int(stream.read().strip())
        if remaining > 0:
            with open(fail_count, "w") as stream:
                stream.write(str(remaining - 1))
            sys.stderr.write("export failed: disk full\n")
            sys.exit(1)
    if active_path and os.path.exists(active_path):
        os.unlink(active_path)
elif method == "exec":
    if active_path and os.path.exists(active_path):
        sys.stderr.write("session is still busy\n")
        sys.exit(1)
    sys.exit(subprocess.run(args[args.index("--") + 1:], cwd=info["view_path"]).returncode)
elif method == "end" and os.getenv("PERSONA_TEST_END_FAIL_REPO") == os.getcwd():
    sys.stderr.write("session still busy; keep the view\n")
    sys.exit(1)
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

func newPersonaHookRepository(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if output, err := exec.Command("git", "-C", path, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("init hook repository: %v %s", err, output)
	}
	return path
}

func personaLeaseRecordEntries(directory string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	leases := make([]os.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "repo-") {
			leases = append(leases, entry)
		}
	}
	return leases, nil
}

func runPersonaWrap(t *testing.T, repoRoot string, event map[string]any, env map[string]string) []byte {
	t.Helper()
	config := make(map[string]string, len(env)+2)
	for key, value := range env {
		config[key] = value
	}
	if config["PERSONA_WRAP_LEASE_DIR"] == "" {
		config["PERSONA_WRAP_LEASE_DIR"] = t.TempDir()
	}
	if err := os.Chmod(config["PERSONA_WRAP_LEASE_DIR"], 0o700); err != nil {
		t.Fatal(err)
	}
	config["PERSONA_WRAP_OWNER_PID"] = strconv.Itoa(os.Getpid())
	if _, exists := event["tool_use_id"]; !exists {
		event["tool_use_id"] = "tool-123"
	}
	if (event["hook_event_name"] == "PostToolUse" || event["hook_event_name"] == "PostToolUseFailure") && config["PERSONA_TEST_SKIP_PRETOOL"] != "1" {
		pre := make(map[string]any, len(event))
		for key, value := range event {
			pre[key] = value
		}
		pre["hook_event_name"] = "PreToolUse"
		runPersonaWrapEvent(t, repoRoot, pre, config)
	}
	return runPersonaWrapEvent(t, repoRoot, event, config)
}

func runPersonaWrapEvent(t *testing.T, repoRoot string, event map[string]any, env map[string]string) []byte {
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
