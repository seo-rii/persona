# persona Claude Code plugin

This directory is a local Claude Code plugin root. It requires Claude Code v2.1.295 or later so `onFailure: "block"` can stop a tool when its preparation hook crashes, times out, or returns invalid output. It is intended for `claude --plugin-dir ./persona-claude-plugin` sessions where Bash should run through persona's daemon-backed overlay view.

## Layout

```
persona-claude-plugin/
  .claude-plugin/plugin.json
  hooks/hooks.json
  bin/persona-wrap
  agents/persona-worker.md
  settings.json
```

## Install for a session

1. Build or locate a persona binary.
   `./build.sh` produces `./bin/persona` by default.
2. Start Claude Code with the plugin directory.

   ```
   claude --plugin-dir ./persona-claude-plugin
   ```

3. When Claude Code enables the plugin, set `PERSONA_BIN` to either:
   - `persona` if the binary is already on `PATH`
   - an absolute path such as `/abs/path/to/persona`
   - a project-relative path such as `./bin/persona`
4. Optional plugin settings can override daemon defaults for every wrapped session:
   - `DAEMON_BASE_MODE`
   - `DAEMON_BASE_REF`
   - `DAEMON_ALLOW_DIRTY`
   - `DAEMON_IGNORED_MODE`
   - `DAEMON_IGNORED_MAX`
   - `DAEMON_APPLY_MODE`
   - `FLUSH_MIN_AGE`
   - `FLUSH_RETRY_FOR`
   - `FLUSH_RETRY_INTERVAL_MS`

The plugin's `settings.json` sets the main thread agent to `persona-worker`.

## Behavior

- `PreToolUse` on `Bash` runs `bin/persona-wrap`, which rewrites eligible Bash calls to `persona daemon exec --session-key <claude-session-id> -- <selected shell> ...`.
- `PreToolUse` on `Read`, `Edit`, `MultiEdit`, `Write`, `NotebookEdit`, `Glob`, and `Grep` rewrites repo-scoped paths into the daemon session's stable `view_path`.
- Managed file tools acquire a mutation lease before the tool runs, including read/search tools so ignored-file masks and cleanup use the same boundary as Bash. The daemon marks the session dirty when acquiring that lease.
- `PostToolUse` and `PostToolUseFailure` release the recorded lease with its `view_generation`; release durably flushes the patch, including partial writes from a failed tool. The `PermissionDenied` hook also releases leases for denials that Claude reports through that event.
- Routing and bypass decisions do not return `permissionDecision: allow`; Claude's usual permission rules and prompts still apply.
- `SessionEnd` runs `persona daemon end --session-key <claude-session-id>` in every repository registered by that chat, including repositories used only through Bash. Cleanup also works when the final event cwd is outside Git.
- The first wrapped Bash call lazily starts a per-repo daemon under `<gitdir>/persona/daemon/` and reuses it for later sessions in that repository.
- Each Claude chat session key maps to its own daemon-backed patch/view pair.
- Repo-scoped write tools are allowed, but writes outside the current repository are denied because they cannot be backed by persona's patch store.
- `.git` and daemon state paths are denied even for read-only file tools so Claude cannot poke at masked Git metadata or session patch internals; the deny reason tells Claude whether it should switch to Git-aware commands or `persona daemon` commands.
- Single literal commands for `git`, `gh`, `persona`, or `claude` may bypass persona, and the bypass context explains that persona hides `.git` from child commands. Shell operators, redirection, active expansions, command substitution, and heredocs are rejected on this bypass path. Split Git inspection and file mutation into separate calls. Quoted literal punctuation is allowed; wrappers such as `env`, `sudo`, and nested shells run wholly through persona.
- Missing `session_id` or file-tool `tool_use_id` is rejected instead of sharing a fallback session. Repository discovery removes inherited `GIT_*` variables, matching the CLI.
- The wrapper uses `tool_input.shell` when Claude exposes it, otherwise falls back to `event.shell`, then the hook process `SHELL`, and finally `bash`.

## Session lifecycle

- A single Claude chat reuses the same `session_id`, so repeated wrapped Bash calls keep writing back into the same daemon session patch.
- Parallel chats in the same Claude Code instance get different `session_id` values, which keeps their patches and views isolated.
- To inspect the stable view or patch path manually, run `persona daemon info --session-key <claude-session-id> --json`.
- To inspect every daemon session for the current repository, run `persona daemon list --json`.
- To persist daemon-backed file-tool changes on demand, run `persona daemon flush --session-key <claude-session-id>`.
- To remove stale sessions in bulk, run `persona daemon prune --idle-for 24h`.
- To flush and fully close a session, run `persona daemon end --session-key <claude-session-id>`. Closing a busy or recovery-required session is refused to preserve its state.
- Normal Claude chat shutdown also triggers `persona daemon end --session-key <claude-session-id>` through the plugin's `SessionEnd` hook. A failed or corrupt repository registration is retained and reported while the hook continues cleaning up other repositories. Successful registrations are removed; failed ones can be retried with another `SessionEnd` event or an explicit daemon end after resolving the failure.
- If you want to reuse a session key with different daemon options, you must end the old session first.

## Optional environment overrides

- `PERSONA_WRAP_PERSONA_BIN` overrides the configured persona binary path.
- `PERSONA_WRAP_BYPASS_PREFIXES` replaces the default bypass prefix list (`git,gh,persona,claude`) with a comma-separated list.
- `PERSONA_WRAP_BYPASS_REGEX` selects additional bypass candidates. Matching commands must still be a single literal shell command; it cannot authorize compound commands.
- `PERSONA_WRAP_DAEMON_TIMEOUT` bounds each daemon subprocess (default `30s`); `SessionEnd` shares that total budget across its registered repositories so a stalled daemon cannot consume every cleanup attempt. Hooks allow `40s` so timeout errors can be reported.
- `PERSONA_WRAP_LEASE_DIR` selects a private record directory (default `${TMPDIR:-/tmp}/persona-claude-<uid>`, current-user ownership and mode `0700` required). Lease records have names derived from repository identity, session ID, and tool-use ID; repository registrations use a `repo-` prefix. Both have mode `0600` and are atomically published after fsync. Keep this directory stable throughout the chat so completion hooks can locate earlier acquisitions.
- `PERSONA_WRAP_OWNER_PID` optionally identifies a live, long-lived Claude owner process for nonstandard launchers. Normally the hook discovers the Claude ancestor through `/proc`; it never uses the short-lived hook process as lease owner.
- Claude plugin options `DAEMON_BASE_MODE`, `DAEMON_BASE_REF`, `DAEMON_ALLOW_DIRTY`, `DAEMON_IGNORED_MODE`, `DAEMON_IGNORED_MAX`, and `DAEMON_APPLY_MODE` are forwarded to both `persona daemon exec` and `persona daemon acquire`. `DAEMON_BASE_MODE` defaults to `worktree`, which pins a detached immutable base; `repo` must be selected explicitly.
- `FLUSH_MIN_AGE`, `FLUSH_RETRY_FOR`, and `FLUSH_RETRY_INTERVAL_MS` remain accepted configuration names for compatibility, but file-tool leases always flush immediately on release. No skipped flush is presented as a scheduled or deferred save. Use explicit daemon commands to request other flush policies.

## Limits

- Repo-scoped file tools are redirected into persona's daemon view, but tool responses may still show internal daemon `view_path` paths because Claude sees the rewritten absolute path.
- After managed file-tool calls, the hook adds mapping context such as `Treat matching paths under <view_path> as repository paths under <repo_root>`, which helps Claude reason about those internal paths even though the tool output itself is unchanged.
- The automatic wrapper defaults to `--base-mode worktree`. Set the `DAEMON_*` plugin options or use explicit daemon commands for other session settings.
- Bypassed Git-oriented shell commands see the original checkout. Git aliases, hooks, external commands, and direct `persona`/`claude` invocations can have side effects; bypass is explicit convenience for trusted commands, not a security sandbox.
- Writes outside the current repository are denied instead of being sent through persona.
- The wrapper no-ops outside git repositories because persona requires a repo.
- The hook checks the translated view target, existing ancestors, and realpath aliases before allowing a file tool, including aliases into `.git` or daemon state. A hook cannot pin the later file open: another process can swap a symlink after the check. File-tool rewriting therefore protects against static escaping paths, not hostile concurrent filesystem changes. Stronger guarantees require an open-time broker or directory-FD based file tool.
- A lease stays busy while a permission prompt is pending. According to the [Claude hooks reference](https://code.claude.com/docs/en/hooks#permissiondenied), `PermissionDenied` only covers auto-mode denials; manual refusal and matching deny rules may not produce a release event. Such a lease remains busy rather than risking concurrent edits. Inspect the private record and run `persona daemon release --session-key <session_id> --lease-id <lease_id> --view-generation <view_generation>` only after confirming that tool is no longer running.
- Release failures are reported to both the user (`systemMessage`) and Claude (`additionalContext` for post-tool hooks). The record remains available for retry; do not end/prune/discard the session while its save is unconfirmed. A restart never causes the hook to flush a replacement generation as though it were the old tool's view.
- Post-tool hooks find the acquired repository and persona executable through the private records, even if the event cwd or binary option changed. Ambiguous tool-use IDs across multiple repositories are refused and their records retained for recovery.
- persona itself still requires Linux, OverlayFS, mount namespaces, and the relevant capabilities.

## Codex note

The repository does not ship an equivalent Codex auto-wrap plugin. Current Codex hooks can block Bash but cannot transparently rewrite tool input the way Claude Code `PreToolUse` hooks can, so Codex support still needs a lower-level runtime or a deny-only workflow.
