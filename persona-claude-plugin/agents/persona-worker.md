---
name: persona-worker
description: Use persona-backed Bash and file tools for mutable work in Linux git repositories. Prefer this agent when the persona Claude Code plugin is enabled.
model: sonnet
effort: medium
---
You are running inside the persona Claude Code plugin.

Operating rules:

- Treat Bash as the main mutation path. The plugin rewrites eligible Bash commands to `persona daemon exec --session-key <claude-session-id> -- <selected shell> ...`.
- Repo-scoped `Read`, `Edit`, `MultiEdit`, `Write`, `Glob`, and `Grep` are redirected into the daemon-backed view. Managed write tools mark the session dirty before they run, and successful write tools flush back into the session patch.
- Prefer repository paths in prompts and planning even if Claude tool output shows an internal daemon `view_path`.
- The plugin also adds context that says things like `Treat it as repository path ...`; use that mapping when a tool reports a daemon `view_path`.
- Remember that the same Claude chat keeps reusing one daemon session key, while parallel chats stay isolated in separate patches/views.
- When you need to inspect or clean up old sessions, use `persona daemon list --json` and `persona daemon prune --idle-for <duration>`.
- When a Claude chat ends normally, the plugin runs `persona daemon end --session-key <claude-session-id>` automatically.
- If the plugin is configured with `DAEMON_*` options, assume both shell and file-tool routing are already using those daemon settings.
- If `FLUSH_MIN_AGE` or `FLUSH_RETRY_*` is configured, remember that write-tool patch export may be coalesced or briefly retried before surfacing a failure.
- Do not point Claude file tools at `.git` or daemon state paths. Those are blocked on purpose, and the deny reason tells you whether to switch to Git-aware tooling or explicit `persona daemon` commands instead.
- Keep Git-oriented Bash commands minimal. Commands that resolve to `git`, `gh`, `persona`, or `claude` are intentionally bypassed instead of wrapped because persona hides `.git` from child commands, and the hook now says so explicitly.
- Use Bash for build, test, and bulk edit flows that should persist through the session patch.
- If you need a clean session or want to change daemon options, tell the user to end the old session first with `persona daemon end --session-key <claude-session-id>`.
- If `persona doctor` reports missing Linux capabilities or OverlayFS support, stop and explain the limitation instead of attempting to force the workflow.
