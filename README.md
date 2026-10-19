# persona

`persona` is a CLI that treats a single patch file as a persistent state store. Run a command in an overlay Git view backed by a patch file. It runs a command inside a Git repository view where the patch is already applied (OverlayFS + mount namespace), then exports changes back into the same patch file (relative to an immutable base OID) atomically.

## Requirements

- Linux with OverlayFS support
- Ability to create mount namespaces and mount OverlayFS (typically CAP_SYS_ADMIN)
- Git available in PATH
- Go 1.25+ to build

## Build

```
cd /path/to/persona
go build ./cmd/persona
```

Helper script:

```
./build.sh
```

Output directory can be customized via `PERSONA_BUILD_DIR` (default: `./bin`).
`build.sh` builds into `./bin` by default and may try to apply `setcap` (or `sudo setcap`) to the resulting binary.
`build.sh` checks `go env GOVERSION` up front and fails early unless the detected toolchain is Go 1.25+.
If `setcap` lives outside the standard trusted absolute paths, set `PERSONA_SETCAP_BIN=/absolute/or/explicit/path/to/setcap`; `build.sh`, `persona doctor`, and `persona activate` all honor the same override.

## Claude Code Plugin

The repository ships a local Claude Code plugin root in `./persona-claude-plugin`.

```
claude --plugin-dir ./persona-claude-plugin
```

When Claude Code enables the plugin, set `PERSONA_BIN` to the built persona binary (`persona`, `/abs/path/to/persona`, or `./bin/persona`).

What the plugin does:

- `PreToolUse` on `Bash` rewrites eligible shell commands to `persona daemon exec --session-key <claude-session-id> -- <selected shell> ...`.
- Repo-scoped `Read`, `Edit`, `MultiEdit`, `Write`, `NotebookEdit`, `Glob`, and `Grep` calls are rewritten into the daemon session's stable `view_path`.
- Managed file tools use `persona daemon acquire` and `persona daemon release` for the same exclusive mutation lease as Bash, with a `view_generation` tied to their view. `PostToolUse`, `PostToolUseFailure`, and `PermissionDenied` hooks release the lease and durably save any changes. A failed save retains recovery state and reports an unsaved-change message to both the user and Claude.
- `SessionEnd` runs `persona daemon end --session-key <claude-session-id>` for every repository used by that chat, even when its final cwd is outside Git. Failed cleanup records remain available for retry.
- The first wrapped Bash call lazily starts a per-repo background daemon under `<gitdir>/persona/daemon/`.
- Each Claude chat session key maps to its own daemon-backed patch/view pair, so concurrent chats in the same Claude instance stay isolated from each other.
- The plugin ships a `persona-worker` agent, and `settings.json` sets `"agent": "persona-worker"` so the main thread prefers Bash-based editing.
- Repo-scoped write tools are allowed; writes outside the current repository are denied because they cannot be backed by persona's patch store.
- Claude file tools are also denied for `.git` and daemon state paths, even when they fall inside the repository tree; the deny message points Claude toward Git-aware commands or explicit `persona daemon` subcommands.
- A single literal command for `git`, `gh`, `persona`, or `claude` may bypass wrapping. Compound commands, redirects, substitutions, and heredocs cannot use this bypass; split Git-aware operations into separate tool calls. Wrappers such as `env`, `sudo`, and nested shells remain inside persona. The hook explains that `.git` is masked from persona child commands. Rewriting or bypassing a command does not emit `permissionDecision: allow`; normal tool permission checks remain in force.
- The wrapper uses the selected shell from the hook payload when available, then falls back to the hook process `SHELL`, and only then to `bash`.

Recommended setup:

1. Build persona and confirm the host can run it.

   ```
   ./build.sh
   ./bin/persona doctor
   ```

2. If you want capability-based execution, run `sudo ./bin/persona activate` once for that binary.
3. Start Claude Code with `claude --plugin-dir ./persona-claude-plugin`.
4. Point plugin config `PERSONA_BIN` at the built persona binary.
5. If the default daemon settings are not enough, set plugin options such as `DAEMON_BASE_MODE`, `DAEMON_BASE_REF`, `DAEMON_ALLOW_DIRTY`, `DAEMON_IGNORED_MODE`, `DAEMON_IGNORED_MAX`, `DAEMON_APPLY_MODE`, `FLUSH_MIN_AGE`, `FLUSH_RETRY_FOR`, and `FLUSH_RETRY_INTERVAL_MS`.

Important limits:

- Repo-scoped Claude file tools now run inside persona's stable daemon view, but their tool responses may still show internal daemon `view_path` absolute paths because Claude sees the rewritten input path.
- The plugin adds path-mapping context after managed file-tool calls so Claude can relate internal daemon paths back to repository paths, but the underlying tool output still uses the executed absolute path.
- Daemon sessions and the plugin default to `--base-mode worktree`, with a frozen base commit. Uncommitted, untracked, and ignored files from the original checkout are not copied into this worktree. Explicit `repo` mode uses the original checkout as a mutable lower layer and requires that it remain unchanged for the entire session.
- A permitted single Git-aware bypass runs against the original repository and may change it. Use it deliberately; session changes are otherwise saved in the patch.
- Writes outside the current repository are denied instead of being sent through persona.
- File-tool path checks reject symlink targets outside the view and aliases of protected state. These hook checks cannot eliminate symlink replacement races between the hook and the external tool opening the file. A tool permission refusal that fires no post hook leaves the lease busy until explicit release or owner termination; see the plugin README.

## Usage

```
persona [OPTIONS] -- <command> [args...]
```

Example:

```
# create or update a patch while running a command
persona --patch /tmp/state.patch -- sh -c 'echo hello > new.txt'

# re-run with the same patch so the view sees the prior changes
persona --patch /tmp/state.patch -- cat new.txt
```

## Commands

- `persona doctor`: print capability/mount diagnostics, trusted `setcap` path, OverlayFS availability, and `unshare -m true` preflight hints.
- `persona activate`: grant `cap_sys_admin` to the persona binary by default. Use `--binary PATH` to target a different persona executable, and add `--allow-dac-override` only when patch writes must bypass DAC checks.
- `persona daemon exec --session-key <key> -- <command...>`: create or reuse a daemon-backed overlay session, run the command inside its stable view, and flush back into that session's patch file.
- `persona daemon info --session-key <key> --json`: ensure a daemon session exists and print its current `view_path` / `patch_path` plus dirty / flush / recovery metadata for tool integrations.
- `persona daemon list --json`: list daemon sessions for the current repository, including busy / dirty state, created / last-used / last-flushed timestamps, and flush / recovery counters.
- `persona daemon flush --session-key <key> [--min-age <duration>]`: write the current daemon view back into that session's patch file without ending the session, optionally skipping very recent flushes.
- `persona daemon prune --idle-for <duration>`: end idle daemon sessions for the current repository.
- `persona daemon end --session-key <key>`: flush the daemon session and remove its overlay view.
- `persona daemon acquire --session-key <key> --owner-pid <pid> --json`: obtain an exclusive file-tool lease. Keep its `lease_id` and `view_generation`.
- `persona daemon release --session-key <key> --lease-id <id> --view-generation <n>`: release that lease and durably save the view.
- `persona daemon recover --session-key <key> --dry-run`: inspect preserved metadata without changing files or mounts. Omit `--dry-run` to validate ownership, refuse live owners, unmount stale session mounts without lazy detach, and reopen a preserved worktree upper layer.
- `persona version`: print the current persona CLI version.
- Run `persona version` or `persona --version` to see the current CLI version instead of relying on README text.

Example:

```
sudo ./bin/persona activate
./bin/persona doctor
```

If your binary lives on a `nosuid` mount, file capabilities are ignored; use `sudo` or move the binary.

Daemon example:

```
persona daemon info --session-key claude-chat-123 --json
persona daemon list --json
persona daemon exec --session-key claude-chat-123 -- sh -lc 'printf hello > note.txt'
persona daemon flush --session-key claude-chat-123
persona daemon prune --idle-for 24h
persona daemon end --session-key claude-chat-123
```

`persona daemon exec` and `persona daemon info` accept the same `--base-mode`, `--base-ref`, `--allow-dirty`, `--ignored-mode`, `--ignored-max`, and `--apply-mode` knobs as the one-shot CLI. Reusing the same session key with different daemon option values is rejected until you run `persona daemon end` for that key.
`persona daemon flush` accepts `--min-age` to skip a recent write-back. A skip does not schedule a future save; dirty data is still unsaved. Lease release and normal shutdown save without this skip.
If the background daemon exits or restarts, the next `persona daemon info`, `list`, `flush`, `prune`, or `end` call recreates persisted sessions from metadata under `<gitdir>/persona/daemon/state/` and resumes from the same patch file and saved base OID. Dirty sessions retain their upper layer and report `recovery_required` when ownership, mounts, a live writer, or a mutable repo-mode lower layer prevents safe restoration. Legacy metadata without a base OID is preserved for manual inspection. Recovery keeps the patch path stable, but the recreated daemon `view_path` may change across daemon restarts.
`persona daemon info --json` and `persona daemon list --json` report `dirty`, `created_*`, `last_used_*`, `last_flushed_*`, `flush_count`, `flush_skipped`, and `recovered_count` so plugin integrations can inspect daemon session health.
The daemon defaults to `--base-mode worktree`. Session info also exposes `base_oid`, `view_generation`, mutation `generation`, `last_durable_generation`, `state`, `last_error`, and `recovery_root`. A normal SIGTERM/SIGINT blocks new operations, allows outstanding lease releases for up to 30 seconds, and saves idle dirty sessions. A lease that remains live or a failed save preserves the upper layer, base worktree, and recovery metadata, logs the recovery path, and makes shutdown return an error. SIGKILL recovery guarantees the last successfully saved patch, and retains newer upper-layer data for recovery. After an unclean exit, stale roots and external mount backing remain tracked and block automatic recreation; `recover --dry-run` reports them and explicit `recover` checks ownership and unmounts them before reopening. A changed temporary-directory configuration may require manual inspection of the old external backing.

Runtime metadata records the saved patch's SHA-256 checksum and schema version. Flush records a dirty checkpoint before replacing the patch, so interruption before the final metadata commit preserves the upper layer. Recovery validates a clean checkpoint's checksum under the patch lock before removing stale resources; dirty recovery also verifies the retained base worktree's HEAD, raw file contents, index, and absence of untracked or ignored additions. Temporary metadata files are excluded from recovery, and corrupt sessions do not hide healthy ones. If cleanup partially fails, the session reports `cleanup_required`; retry `daemon end` to finish only the remaining cleanup operations.

Daemon RPCs have a 30-second maximum duration, cancel work on client disconnect, and use per-session locks. The socket uses a short private directory under `/tmp`; protocol ping/pong checks the repository identity. A lifetime lock prevents a second server from unlinking an active socket. Background daemon diagnostics are appended to `<gitdir>/persona/daemon/daemon.log` (mode 0600).

## Options

- `--version`: print the current persona CLI version and exit.
- `--patch PATH`: patch file path (default: auto-generated)
- `--patch-dir PATH`: directory for auto-generated patch files (default: `<GitDir>/persona/patches`)
- `--print-patch-path`: print final patch path on exit

Base options:
- `--base-mode {repo,worktree}` (default: `repo`)
- `--base-ref REF` (worktree only, default: `HEAD`)
- `--allow-dirty` (repo mode only)

Ignored handling:
- `--ignored-mode {transparent,readonly,masked}` (default: `transparent`)
- `--ignored-max N` (default: `200`, `0` disables ignored processing)
  `readonly` and `masked` reject ignored symlinks instead of following their referents.
  If ignored candidates exceed `N`, persona fails instead of silently truncating the set.

Patch apply:
- `--apply-mode {strict,reject}` (default: `strict`)

Session/logging:
- `--keep-session {on-fail,always,never}` (default: `on-fail`)
  `on-fail` keeps the session only when persona itself returns an internal error; a child command exiting non-zero still counts as a completed run and does not keep the session.
- `--verbose`

## Behavior Notes

- `.git` is always masked in the view.
- If the patch file is inside the repo, it is masked in the view and excluded from export.
- Patch locking uses a sibling `<patch>.lock` file. Persona unlocks and closes it after the run, but does not remove the lock file; if the patch lives inside the repo, the lock path is also masked in the view and excluded from export.
- A run resolves its base to an immutable OID before apply/export; worktree mode uses `--base-ref`, while repo mode freezes its starting HEAD. Daemon metadata keeps that OID across restart even when refs move. A per-patch `.base.json` sidecar and `refs/persona/bases/<key-hash>` retain the base across `end`, later reuse, and Git GC; retain them for as long as the patch is needed. Reusing a key with a different base requires a new key. Reusing a standalone patch requires the same base commit.
- Unborn repo mode uses the empty tree and includes staged files. Worktree mode requires a committed base.
- Saved patches fix diff color, prefixes, context, full indexes, and binary representation independently of user display settings. textconv and external diff programs are disabled. The original Git index is never used as the session index.
- Changed files with content-transform attributes (`filter`, `text`, `eol`, `ident`, or `working-tree-encoding`) are explicitly rejected because Git conversion can lose working-file bytes. Unchanged files may remain. Git submodule worktree contents are not preserved by ordinary file patches.
- Ignored untracked files are excluded from export by default.
- If ignored processing is enabled and the ignored path set changes during the run in either direction, export fails.
- Special files (FIFO/device/socket) are skipped with a warning.
- Patch write-back uses an atomic rename and file/directory fsync inside the patch directory. Existing owner/group and permission bits are preserved. Metadata is also fsynced before success is returned. Patch input is a single bounded regular-file snapshot used for both validation and apply. Internal patch/lock/temp file descriptors are close-on-exec.
- Patch files and exported diffs are capped at 16 MiB; larger inputs fail explicitly instead of relying on scanner or memory pressure side effects.
- If persona cannot preserve the existing patch file owner/group during atomic write-back, it fails instead of silently changing ownership.

## Isolation and preservation contract

Persona separates ordinary cooperative repository work; it is not a security sandbox for hostile processes. Saved file state includes contents, deletion, symlink targets, and the executable bit. Git patches do not retain ownership, timestamps, extended attributes, empty directories, ignored-file contents, or submodule worktree contents.

| Operation | Scope |
| --- | --- |
| Relative paths and original repo absolute paths in `daemon exec` | A private child mount namespace recursively maps the view over the original repo, preserving nested masks. |
| File tools | Exclusive lease, generation check, and target/ancestor symlink checks; external-tool open-time races remain possible. |
| Explicit original Git/CLI bypass | Runs on the original repository with normal tool permissions. |
| Paths outside the repository, external symlinks, network, other processes | Ordinary OS access applies; shell commands can write outside patch coverage. |
| Root/capability execution | Mount capability is required; it does not make arbitrary commands safe. |
| Escaped descendants (`setsid`) | Process-group cleanup cannot guarantee termination of every detached descendant. |
| Ignored files | All managed tools use the same lease masking policy. They are excluded from saved file state; `.gitignore` changes that alter the ignored path set fail export. |

Use `recover --dry-run` before recovery of an uncertain session. A failed save keeps the existing durable patch and preserves unsaved data; keep the reported recovery root until its contents have been reviewed. Automatic recovery does not delete directories named by metadata. Explicit recovery removes only validated, unmounted clean session roots and owned external temporary backing; a dirty upper layer remains intact. Restoring a dirty worktree requires that its recorded base worktree has also been retained; dirty repo-mode sessions need manual recovery because their mutable lower cannot be verified.

## Implementation Notes (Spec Deltas)

- `.git` and patch file masking are applied **only during the command execution** and unmounted before export/write-back. This keeps git operations working while still hiding `.git` from the command.
- Internal git operations ignore any pre-set `GIT_DIR`/`GIT_WORK_TREE` environment variables and always use the detected repo. Child commands also run with ambient `GIT_*` variables removed.
- Patch paths are resolved through symlinks. If the provided patch path is a symlink to a repo-internal file, the tool locks/writes the **target path**, and `--print-patch-path` prints the resolved path.
- Existing parent directories for `--patch` and `--patch-dir` must not be symlinks. Symlink patch files are allowed; symlink parent directories are rejected.
- Internal git operations use a bind-mounted gitdir outside the repo root to avoid `.git` masking interfering with export/apply.
- One-shot namespace setup, command execution, and mount cleanup run on a dedicated locked OS thread. The thread is retired when the goroutine exits and is never returned to the scheduler with a private namespace.
- In `strict` apply mode, if a text patch adds a new regular file that already exists with identical content and mode, persona skips that block during apply so re-running the same text new-file patch is idempotent. Binary new-file blocks are retried normally.

## Exit Codes

- `0` success (patch write-back attempted regardless of command exit)
- `10` environment/mount error
- `11` repo state error
- `12` patch apply failure
- `13` export failure
- `14` patch write failure
- Child exit codes propagate unchanged, except child `10`~`14`, which are remapped to `240`~`244` to avoid colliding with persona's own failure codes.

## Integration Tests

Integration tests require Linux + root for mount namespace/OverlayFS. In non-privileged environments they may compile and skip without exercising the real mount lifecycle.

```
cd /path/to/persona
sudo env "PATH=$PATH" PERSONA_INTEGRATION=1 $(command -v go) test -count=1 ./integration
```

If `go` is not in the sudo PATH, keep the `env "PATH=$PATH"` prefix.

On hosts permitting unprivileged user namespaces, the same suite can run with `unshare -Ur -m --propagation private env PERSONA_INTEGRATION=1 "$(command -v go)" test -count=1 ./integration`. Required integration runs fail if mount setup is unavailable. The suite isolates its mount namespace and tests actual daemon shutdown/recovery, shared leases, and plugin file-tool round trips against disposable repositories.

After changes around mount/masking, linked worktree handling, or patch export/apply boundaries, re-run the privileged integration suite with `PERSONA_INTEGRATION=1` to confirm the real mount-namespace path still behaves as expected.

## Test Helpers

```
./test.sh
./test_log_stderr.sh [/tmp/persona-test.log]
```

`test.sh` runs unit tests (`./cmd/... ./internal/...`) then integration tests.

`test_log_stderr.sh` runs unit + integration tests, appends a timestamp to the log filename, writes all output to a single log, and prints a summary at the end.
