//go:build linux

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type claudeHookResponse struct {
	Output struct {
		Event        string         `json:"hookEventName"`
		Decision     string         `json:"permissionDecision"`
		Context      string         `json:"additionalContext"`
		UpdatedInput map[string]any `json:"updatedInput"`
	} `json:"hookSpecificOutput"`
	Warning string `json:"systemMessage"`
}

func TestPersonaIntegrationClaudePluginFileToolRoundTrip(t *testing.T) {
	binary := requireIntegration(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatalf("required Claude plugin integration needs python3: %v", err)
	}
	hook := filepath.Join(repoRoot(), "persona-claude-plugin", "bin", "persona-wrap")
	for _, writer := range []struct{ tool, pathKey, name string }{
		{"Write", "file_path", "tracked.txt"},
		{"NotebookEdit", "notebook_path", "analysis.ipynb"},
	} {
		for _, completion := range []string{"PostToolUse", "PostToolUseFailure"} {
			t.Run(writer.tool+"/"+completion, func(t *testing.T) {
				repo := createRepo(t)
				original := filepath.Join(repo, writer.name)
				want := []byte("saved through the real Claude hook\r\nincluding partial tool changes\r\n")
				toolInput := map[string]any{writer.pathKey: original, "content": string(want)}
				if writer.tool == "NotebookEdit" {
					base := []byte(`{"cells":[{"cell_type":"code","execution_count":null,"id":"cell-1","metadata":{},"outputs":[],"source":["print('base')\n"]}],"metadata":{},"nbformat":4,"nbformat_minor":5}` + "\n")
					writeFile(t, original, base)
					runCmd(t, repo, "git", "add", writer.name)
					runCmd(t, repo, "git", "commit", "-m", "add notebook fixture")
					want = bytes.Replace(base, []byte("print('base')"), []byte("print('saved')"), 1)
					toolInput = map[string]any{writer.pathKey: original, "cell_id": "cell-1", "new_source": "print('saved')", "edit_mode": "replace"}
				}
				leaseDir := t.TempDir()
				if err := os.Chmod(leaseDir, 0o700); err != nil {
					t.Fatal(err)
				}
				daemon := startManagedTestDaemon(t, binary, repo)
				const key = "claude-plugin-roundtrip"
				runHook := func(event map[string]any) claudeHookResponse {
					t.Helper()
					input, err := json.Marshal(event)
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, hook)
					cmd.Dir = repo
					cmd.Env = append(os.Environ(),
						"PERSONA_WRAP_PERSONA_BIN="+binary,
						"PERSONA_WRAP_OWNER_PID="+strconv.Itoa(os.Getpid()),
						"PERSONA_WRAP_LEASE_DIR="+leaseDir,
						"PERSONA_WRAP_DAEMON_TIMEOUT=20s",
						"CLAUDE_PROJECT_DIR="+repo,
						"CLAUDE_PLUGIN_OPTION_DAEMON_BASE_MODE=worktree",
						"CLAUDE_PLUGIN_OPTION_DAEMON_BASE_REF=HEAD",
						"CLAUDE_PLUGIN_OPTION_DAEMON_ALLOW_DIRTY=false",
						"CLAUDE_PLUGIN_OPTION_DAEMON_IGNORED_MODE=transparent",
						"CLAUDE_PLUGIN_OPTION_DAEMON_IGNORED_MAX=200",
						"CLAUDE_PLUGIN_OPTION_DAEMON_APPLY_MODE=strict",
					)
					cmd.Stdin = bytes.NewReader(input)
					var stdout, stderr bytes.Buffer
					cmd.Stdout, cmd.Stderr = &stdout, &stderr
					if err := cmd.Run(); err != nil {
						t.Fatalf("%s hook failed: %v; stdout=%s stderr=%s", event["hook_event_name"], err, &stdout, &stderr)
					}
					if stderr.Len() != 0 {
						t.Fatalf("%s hook reported failure: %s", event["hook_event_name"], &stderr)
					}
					var response claudeHookResponse
					if stdout.Len() != 0 {
						if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
							t.Fatalf("decode hook response: %v: %s", err, &stdout)
						}
					}
					if response.Warning != "" || response.Output.Decision != "" {
						t.Fatalf("hook did not preserve the tool and its permission policy: %s", &stdout)
					}
					return response
				}
				// NotebookEdit follows a managed Read, matching Claude's notebook workflow.
				if writer.tool == "NotebookEdit" {
					before := map[string]any{
						"session_id": key, "tool_use_id": "read-before-notebook", "cwd": repo,
						"hook_event_name": "PreToolUse", "tool_name": "Read",
						"tool_input": map[string]any{"file_path": original},
					}
					read := runHook(before)
					path, _ := read.Output.UpdatedInput["file_path"].(string)
					if !bytes.Equal(readFile(t, path), readFile(t, original)) {
						t.Fatal("initial notebook Read changed the base contents")
					}
					before["hook_event_name"], before["tool_input"] = "PostToolUse", read.Output.UpdatedInput
					runHook(before)
				}
				event := map[string]any{
					"session_id": key, "tool_use_id": "write-1", "cwd": repo,
					"hook_event_name": "PreToolUse", "tool_name": writer.tool,
					"tool_input": toolInput,
				}
				prepared := runHook(event)
				path, _ := prepared.Output.UpdatedInput[writer.pathKey].(string)
				leased := daemonInfo(t, binary, repo, key)
				if path != filepath.Join(leased.ViewPath, writer.name) || path == original || !leased.Busy || !leased.Dirty {
					t.Fatalf("PreToolUse did not acquire and route into the live view: path=%q session=%#v", path, leased)
				}
				daemonMustFail(t, binary, repo, "already executing", "exec", "--session-key", key, "--", "true")
				// This is the actual file-tool write, between the real pre/post hooks.
				writeFile(t, path, want)
				assertOriginalCheckout(t, repo)
				event["hook_event_name"], event["tool_input"] = completion, prepared.Output.UpdatedInput
				event["cwd"] = t.TempDir()
				if saved := runHook(event); !strings.Contains(saved.Output.Context, "durably flushed") {
					t.Fatalf("completion did not confirm persistence: %#v", saved)
				}
				checkpoint := daemonInfo(t, binary, repo, key)
				if checkpoint.Busy || checkpoint.Dirty || checkpoint.Generation != checkpoint.LastDurableGeneration {
					t.Fatalf("completion did not establish a durable checkpoint: %#v", checkpoint)
				}
				if got := readFile(t, filepath.Join(materializeDaemonPatch(t, repo, checkpoint), writer.name)); !bytes.Equal(got, want) {
					t.Fatalf("hook-exported patch did not reproduce file-tool bytes: %q", got)
				}
				daemon.stop()
				daemon = startManagedTestDaemon(t, binary, repo)
				event = map[string]any{
					"session_id": key, "tool_use_id": "read-2", "cwd": repo,
					"hook_event_name": "PreToolUse", "tool_name": "Read",
					"tool_input": map[string]any{"file_path": original},
				}
				prepared = runHook(event)
				path, _ = prepared.Output.UpdatedInput["file_path"].(string)
				recovered := daemonInfo(t, binary, repo, key)
				if recovered.ViewGeneration <= leased.ViewGeneration || recovered.BaseOID != leased.BaseOID || path != filepath.Join(recovered.ViewPath, writer.name) {
					t.Fatalf("Read did not route into the recovered generation: path=%q session=%#v", path, recovered)
				}
				if got := readFile(t, path); !bytes.Equal(got, want) {
					t.Fatalf("Read after daemon restart lost file-tool bytes: %q", got)
				}
				event["hook_event_name"], event["tool_input"] = "PostToolUse", prepared.Output.UpdatedInput
				runHook(event)
				runHook(map[string]any{"session_id": key, "cwd": t.TempDir(), "hook_event_name": "SessionEnd"})
				if entries, err := os.ReadDir(leaseDir); err != nil || len(entries) != 0 {
					t.Fatalf("SessionEnd left private hook records behind: %v %v", entries, err)
				}
				if _, err := os.Stat(recovered.ViewPath); !os.IsNotExist(err) {
					t.Fatalf("SessionEnd did not remove its view: %v", err)
				}
				daemon.stop()
				assertOriginalCheckout(t, repo)
			})
		}
	}
}
