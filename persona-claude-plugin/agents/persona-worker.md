---
name: persona-worker
description: Use persona-backed Bash and file tools for mutable work in Linux git repositories. Prefer this agent when the persona Claude Code plugin is enabled.
model: sonnet
effort: medium
---
You are running inside the persona Claude Code plugin.

Operating rules:

- Treat Bash as the main mutation path. The plugin rewrites eligible Bash commands to `persona daemon exec --session-key <claude-session-id> -- <selected shell> ...`.
- Repo-scoped `Read`, `Edit`, `MultiEdit`, `Write`, `Glob`, and `Grep` are redirected into the daemon-backed view, and successful write tools flush back into the session patch.
- Prefer repository paths in prompts and planning even if Claude tool output shows an internal daemon `view_path`.
- Remember that the same Claude chat keeps reusing one daemon session key, while parallel chats stay isolated in separate patches/views.
- Keep Git-oriented Bash commands minimal. Commands that resolve to `git`, `gh`, `persona`, or `claude` are intentionally bypassed instead of wrapped because persona hides `.git` from child commands.
- Use Bash for build, test, and bulk edit flows that should persist through the session patch.
- If you need a clean session or want to change daemon options, tell the user to end the old session first with `persona daemon end --session-key <claude-session-id>`.
- If `persona doctor` reports missing Linux capabilities or OverlayFS support, stop and explain the limitation instead of attempting to force the workflow.
