# persona Claude Code plugin

This directory is a local Claude Code plugin root. It is intended for `claude --plugin-dir ./persona-claude-plugin` sessions where Bash should run through persona's patch-backed overlay view.

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

- `PreToolUse` on `Bash` runs `bin/persona-wrap`, which rewrites eligible Bash calls to `persona --patch <session-repo patch> -- <selected shell> ...`.
- Patch files live under `${CLAUDE_PLUGIN_DATA}/patches/` by default and are keyed by the Claude `session_id` plus the current repo root.
- `Edit`, `MultiEdit`, and `Write` are denied so file mutations stay inside the persona-backed Bash path.
- Commands that resolve to `git`, `gh`, `persona`, or `claude` are bypassed instead of wrapped.
- The wrapper uses `tool_input.shell` when Claude exposes it, otherwise falls back to `event.shell`, then the hook process `SHELL`, and finally `bash`.

## Optional environment overrides

- `PERSONA_WRAP_PERSONA_BIN` overrides the configured persona binary path.
- `PERSONA_WRAP_PATCH_ROOT` overrides `${CLAUDE_PLUGIN_DATA}/patches`.
- `PERSONA_WRAP_BYPASS_PREFIXES` replaces the default bypass prefix list (`git,gh,persona,claude`) with a comma-separated list.
- `PERSONA_WRAP_BYPASS_REGEX` bypasses wrapping when the raw Bash command matches the regex.

## Limits

- This plugin only changes Bash execution. Native Claude Code file tools do not see persona's overlay mount.
- Because `Read` observes the checkout on disk, use Bash reads for files that were changed through persona during the same session.
- The wrapper no-ops outside git repositories because persona requires a repo.
- persona itself still requires Linux, OverlayFS, mount namespaces, and the relevant capabilities.

## Codex note

The repository does not ship an equivalent Codex auto-wrap plugin. Current Codex hooks can block Bash but cannot transparently rewrite tool input the way Claude Code `PreToolUse` hooks can, so Codex support still needs a lower-level runtime or a deny-only workflow.
