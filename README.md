# persona (v0.2.0)

`persona` is a CLI that treats a single patch file as a persistent state store. Run a command in an overlay Git view backed by a patch file. It runs a command inside a Git repository view where the patch is already applied (OverlayFS + mount namespace), then exports changes back into the same patch file (HEAD-based) atomically.

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
If `setcap` lives outside the standard trusted absolute paths, set `PERSONA_SETCAP_BIN=/absolute/or/explicit/path/to/setcap`; `build.sh`, `persona doctor`, and `persona activate` all honor the same override.

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

- `persona doctor`: print capability/mount diagnostics and hints for permission issues.
- `persona activate`: grant `cap_sys_admin` to the persona binary by default. Use `--binary PATH` to target a different persona executable, and add `--allow-dac-override` only when patch writes must bypass DAC checks.
- `persona version`: print the current persona CLI version.

Example:

```
sudo ./bin/persona activate
./bin/persona doctor
```

If your binary lives on a `nosuid` mount, file capabilities are ignored; use `sudo` or move the binary.

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
- `--verbose`

## Behavior Notes

- `.git` is always masked in the view.
- If the patch file is inside the repo, it is masked in the view and excluded from export.
- Patch locking uses a sibling `<patch>.lock` file. Persona unlocks and closes it after the run, but does not remove the lock file; if the patch lives inside the repo, the lock path is also masked in the view and excluded from export.
- Export is always based on `HEAD`.
- Ignored untracked files are excluded from export by default.
- If ignored processing is enabled and the ignored path set changes during the run in either direction, export fails.
- Special files (FIFO/device/socket) are skipped with a warning.
- Patch write-back uses an atomic rename inside the patch directory.
- Patch files and exported diffs are capped at 16 MiB; larger inputs fail explicitly instead of relying on scanner or memory pressure side effects.
- If persona cannot preserve the existing patch file owner/group during atomic write-back, it fails instead of silently changing ownership.

## Implementation Notes (Spec Deltas)

- `.git` and patch file masking are applied **only during the command execution** and unmounted before export/write-back. This keeps git operations working while still hiding `.git` from the command.
- Internal git operations ignore any pre-set `GIT_DIR`/`GIT_WORK_TREE` environment variables and always use the detected repo. Child commands also run with ambient `GIT_*` variables removed.
- Patch paths are resolved through symlinks. If the provided patch path is a symlink to a repo-internal file, the tool locks/writes the **target path**, and `--print-patch-path` prints the resolved path.
- Existing parent directories for `--patch` and `--patch-dir` must not be symlinks. Symlink patch files are allowed; symlink parent directories are rejected.
- Internal git operations use a bind-mounted gitdir outside the repo root to avoid `.git` masking interfering with export/apply.
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
sudo env "PATH=$PATH" PERSONA_INTEGRATION=1 $(command -v go) test ./integration -run TestPersonaIntegration
```

If `go` is not in the sudo PATH, keep the `env "PATH=$PATH"` prefix.

After changes around mount/masking, linked worktree handling, or patch export/apply boundaries, re-run the privileged integration suite with `PERSONA_INTEGRATION=1` to confirm the real mount-namespace path still behaves as expected.

## Test Helpers

```
./test.sh
./test_log_stderr.sh [/tmp/persona-test.log]
```

`test.sh` runs unit tests (`./cmd/... ./internal/...`) then integration tests.

`test_log_stderr.sh` runs unit + integration tests, appends a timestamp to the log filename, writes all output to a single log, and prints a summary at the end.
