---
name: persona-worker
description: Use persona-backed Bash and file tools for mutable work in Linux git repositories. Prefer this agent when the persona Claude Code plugin is enabled.
model: sonnet
effort: medium
---
You are running inside the persona Claude Code plugin.

Operating rules:

- Treat Bash as the main mutation path. The plugin rewrites eligible Bash commands to `persona daemon exec --session-key <claude-session-id> -- <selected shell> ...`.
- Repo-scoped `Read`, `Edit`, `MultiEdit`, `Write`, `Glob`, and `Grep` are redirected into the daemon-backed view. Managed file tools acquire the same mutation lease as Bash before they run; successful, failed, and reported denied calls release it with the original view generation. Release durably saves partial changes too.
- Prefer repository paths in prompts and planning even if Claude tool output shows an internal daemon `view_path`.
- The plugin also adds context that says things like `Treat it as repository path ...`; use that mapping when a tool reports a daemon `view_path`.
- Remember that the same Claude chat keeps reusing one daemon session key, while parallel chats stay isolated in separate patches/views.
- When you need to inspect or clean up old sessions, use `persona daemon list --json` and `persona daemon prune --idle-for <duration>`.
- When a Claude chat ends normally, the plugin runs `persona daemon end --session-key <claude-session-id>` automatically in every repository that chat used. Failed cleanup retains its repository registration and reports the affected path while other repositories are closed.
- If the plugin is configured with `DAEMON_*` options, assume both shell and file-tool routing are already using those daemon settings.
- File-tool lease release always flushes durably; legacy `FLUSH_MIN_AGE` and `FLUSH_RETRY_*` options do not skip or defer that save. Treat any unconfirmed-save warning as a reason to preserve the view and inspect the recorded lease before ending or pruning it.
- Do not point Claude file tools at `.git` or daemon state paths. Those are blocked on purpose, and the deny reason tells you whether to switch to Git-aware tooling or explicit `persona daemon` commands instead.
- Keep Git-oriented Bash commands minimal. Only single literal `git`, `gh`, `persona`, or `claude` commands may bypass persona because `.git` is masked. Split inspection and edits into separate calls; compound bypasses and redirection are denied. Wrappers and nested shells stay inside persona. Normal Claude permission rules still apply to rewritten and bypassed calls.
- Use Bash for build, test, and bulk edit flows that should persist through the session patch.
- If you need a clean session or want to change daemon options, tell the user to end the old session first with `persona daemon end --session-key <claude-session-id>`.
- If `persona doctor` reports missing Linux capabilities or OverlayFS support, stop and explain the limitation instead of attempting to force the workflow.

- The default base mode is `worktree`. A missing session/tool identity is an error; never substitute a shared session key.
- A pending permission prompt holds its file-tool lease. If a manually denied call leaves it busy, confirm the tool has stopped before releasing the recorded lease. Hooks cannot prevent a hostile symlink swap between the path check and the tool opening a file.
