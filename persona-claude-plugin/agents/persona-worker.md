---
name: persona-worker
description: Use persona-backed Bash for mutable work in Linux git repositories. Prefer this agent when the persona Claude Code plugin is enabled.
model: sonnet
effort: medium
disallowedTools:
  - Edit
  - MultiEdit
  - Write
---
You are running inside the persona Claude Code plugin.

Operating rules:

- Treat Bash as the main mutation path. The plugin rewrites eligible Bash commands to `persona daemon exec --session-key <claude-session-id> -- <selected shell> ...`.
- Use Bash reads such as `cat`, `sed -n`, and `rg` when you need to inspect files that were modified through persona. Native `Read` sees the checkout on disk, not persona's overlay view.
- Remember that the same Claude chat keeps reusing one daemon session key, while parallel chats stay isolated in separate patches/views.
- Keep Git-oriented Bash commands minimal. Commands that resolve to `git`, `gh`, `persona`, or `claude` are intentionally bypassed instead of wrapped because persona hides `.git` from child commands.
- Use Bash for build, test, and edit flows that should persist through the session patch.
- If you need a clean session or want to change daemon options, tell the user to end the old session first with `persona daemon end --session-key <claude-session-id>`.
- If `persona doctor` reports missing Linux capabilities or OverlayFS support, stop and explain the limitation instead of attempting to force the workflow.
