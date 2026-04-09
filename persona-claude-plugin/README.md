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

The plugin's `settings.json` sets the main thread agent to `persona-worker`.

## Behavior

- `PreToolUse` on `Bash` runs `bin/persona-wrap`, which rewrites eligible Bash calls to `persona daemon exec --session-key <claude-session-id> -- <selected shell> ...`.
- The first wrapped Bash call lazily starts a per-repo daemon under `<gitdir>/persona/daemon/` and reuses it for later sessions in that repository.
- Each Claude chat session key maps to its own daemon-backed patch/view pair.
- `Edit`, `MultiEdit`, and `Write` are denied so file mutations stay inside the persona-backed Bash path.
- Commands that resolve to `git`, `gh`, `persona`, or `claude` are bypassed instead of wrapped.
- The wrapper uses `tool_input.shell` when Claude exposes it, otherwise falls back to `event.shell`, then the hook process `SHELL`, and finally `bash`.

## Session lifecycle

- A single Claude chat reuses the same `session_id`, so repeated wrapped Bash calls keep writing back into the same daemon session patch.
- Parallel chats in the same Claude Code instance get different `session_id` values, which keeps their patches and views isolated.
- To inspect the stable view or patch path manually, run `persona daemon info --session-key <claude-session-id> --json`.
- To discard or fully close a session, run `persona daemon end --session-key <claude-session-id>`.
- If you want to reuse a session key with different daemon options, you must end the old session first.

## Optional environment overrides

- `PERSONA_WRAP_PERSONA_BIN` overrides the configured persona binary path.
- `PERSONA_WRAP_BYPASS_PREFIXES` replaces the default bypass prefix list (`git,gh,persona,claude`) with a comma-separated list.
- `PERSONA_WRAP_BYPASS_REGEX` bypasses wrapping when the raw Bash command matches the regex.

## Limits

- This plugin only changes Bash execution. Native Claude Code file tools still do not see persona's overlay mount.
- Because `Read` observes the checkout on disk, use Bash reads for files that were changed through persona during the same session.
- The automatic wrapper uses daemon defaults. If you need `--base-mode worktree` or other daemon flags, run `persona daemon ...` commands manually.
- Git-oriented shell commands are bypassed instead of wrapped, so they do not see the persona overlay view.
- The wrapper no-ops outside git repositories because persona requires a repo.
- persona itself still requires Linux, OverlayFS, mount namespaces, and the relevant capabilities.

## Codex note

The repository does not ship an equivalent Codex auto-wrap plugin. Current Codex hooks can block Bash but cannot transparently rewrite tool input the way Claude Code `PreToolUse` hooks can, so Codex support still needs a lower-level runtime or a deny-only workflow.
