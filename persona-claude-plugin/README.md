# persona Claude Code plugin

This directory is a local Claude Code plugin root. It is intended for `claude --plugin-dir ./persona-claude-plugin` sessions where Bash should run through persona's daemon-backed overlay view.

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

The plugin's `settings.json` sets the main thread agent to `persona-worker`.

## Behavior

- `PreToolUse` on `Bash` runs `bin/persona-wrap`, which rewrites eligible Bash calls to `persona daemon exec --session-key <claude-session-id> -- <selected shell> ...`.
- `PreToolUse` on `Read`, `Edit`, `MultiEdit`, `Write`, `Glob`, and `Grep` rewrites repo-scoped paths into the daemon session's stable `view_path`.
- `PostToolUse` on `Edit`, `MultiEdit`, and `Write` runs `persona daemon flush --session-key <claude-session-id>` so file-tool mutations land in the session patch.
- The first wrapped Bash call lazily starts a per-repo daemon under `<gitdir>/persona/daemon/` and reuses it for later sessions in that repository.
- Each Claude chat session key maps to its own daemon-backed patch/view pair.
- Repo-scoped write tools are allowed, but writes outside the current repository are denied because they cannot be backed by persona's patch store.
- `.git` and daemon state paths are denied even for read-only file tools so Claude cannot poke at masked Git metadata or session patch internals.
- Commands that resolve to `git`, `gh`, `persona`, or `claude` are bypassed instead of wrapped.
- The wrapper uses `tool_input.shell` when Claude exposes it, otherwise falls back to `event.shell`, then the hook process `SHELL`, and finally `bash`.

## Session lifecycle

- A single Claude chat reuses the same `session_id`, so repeated wrapped Bash calls keep writing back into the same daemon session patch.
- Parallel chats in the same Claude Code instance get different `session_id` values, which keeps their patches and views isolated.
- To inspect the stable view or patch path manually, run `persona daemon info --session-key <claude-session-id> --json`.
- To inspect every daemon session for the current repository, run `persona daemon list --json`.
- To persist daemon-backed file-tool changes on demand, run `persona daemon flush --session-key <claude-session-id>`.
- To remove stale sessions in bulk, run `persona daemon prune --idle-for 24h`.
- To discard or fully close a session, run `persona daemon end --session-key <claude-session-id>`.
- If you want to reuse a session key with different daemon options, you must end the old session first.

## Optional environment overrides

- `PERSONA_WRAP_PERSONA_BIN` overrides the configured persona binary path.
- `PERSONA_WRAP_BYPASS_PREFIXES` replaces the default bypass prefix list (`git,gh,persona,claude`) with a comma-separated list.
- `PERSONA_WRAP_BYPASS_REGEX` bypasses wrapping when the raw Bash command matches the regex.
- Claude plugin options `DAEMON_BASE_MODE`, `DAEMON_BASE_REF`, `DAEMON_ALLOW_DIRTY`, `DAEMON_IGNORED_MODE`, `DAEMON_IGNORED_MAX`, and `DAEMON_APPLY_MODE` are forwarded to both `persona daemon exec` and `persona daemon info`.

## Limits

- Repo-scoped file tools are redirected into persona's daemon view, but tool responses may still show internal daemon `view_path` paths because Claude sees the rewritten absolute path.
- After managed file-tool calls, the hook adds mapping context such as `Treat matching paths under <view_path> as repository paths under <repo_root>`, which helps Claude reason about those internal paths even though the tool output itself is unchanged.
- The automatic wrapper uses daemon defaults. If you need `--base-mode worktree` or other daemon flags, run `persona daemon ...` commands manually.
- Git-oriented shell commands are bypassed instead of wrapped, so they do not see the persona overlay view.
- Writes outside the current repository are denied instead of being sent through persona.
- The wrapper no-ops outside git repositories because persona requires a repo.
- persona itself still requires Linux, OverlayFS, mount namespaces, and the relevant capabilities.

## Codex note

The repository does not ship an equivalent Codex auto-wrap plugin. Current Codex hooks can block Bash but cannot transparently rewrite tool input the way Claude Code `PreToolUse` hooks can, so Codex support still needs a lower-level runtime or a deny-only workflow.
