package gitx

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"persona/internal/model"
)

type Git struct {
	RepoRoot string
	GitDir   string
	Verbose  bool
	// BaseOID is the immutable commit (or unborn repository's empty tree) used
	// for both restoration and export. ResolveBase binds it once per session.
	BaseOID string
}

type alreadyExistsApplyError struct {
	error
}

func (alreadyExistsApplyError) AlreadyExists() bool {
	return true
}

// Compile-time check that Git satisfies model.GitOps.
var _ model.GitOps = (*Git)(nil)

// RepoRootPath returns the repository root directory.
func (g *Git) RepoRootPath() string { return g.RepoRoot }

// GitDirPath returns the .git directory path.
func (g *Git) GitDirPath() string { return g.GitDir }

// SetBaseOID supplies the persisted base before ResolveBase validates it.
func (g *Git) SetBaseOID(oid string) { g.BaseOID = oid }

func (g *Git) PinnedBaseOID() string { return g.BaseOID }

func DetectRepo(ctx context.Context, cwd string) (string, string, error) {
	env := FilterGitEnv(os.Environ())
	repoRoot, err := gitOutput(ctx, cwd, env, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", err
	}
	gitDir, err := gitOutput(ctx, cwd, env, "git", "rev-parse", "--git-dir")
	if err != nil {
		return "", "", err
	}
	repoRoot = strings.TrimSpace(repoRoot)
	gitDir = strings.TrimSpace(gitDir)
	if repoRoot == "" || gitDir == "" {
		return "", "", fmt.Errorf("git rev-parse returned empty")
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repoRoot, gitDir)
	}
	return repoRoot, gitDir, nil
}

func (g Git) IsClean(ctx context.Context) (bool, error) {
	return g.IsCleanExceptUntracked(ctx, nil)
}

func (g Git) IsCleanExceptUntracked(ctx context.Context, ignoreUntracked []string) (bool, error) {
	clean, err := g.diffQuiet(ctx)
	if err != nil || !clean {
		return clean, err
	}
	clean, err = g.diffCachedQuiet(ctx)
	if err != nil || !clean {
		return clean, err
	}
	hasUntracked, err := g.hasUntrackedExcept(ctx, ignoreUntracked)
	if err != nil {
		return false, err
	}
	if hasUntracked {
		return false, nil
	}
	return true, nil
}

func (g Git) IsCleanExceptPaths(ctx context.Context, excludePaths []string) (bool, error) {
	clean, err := g.diffQuietExcept(ctx, excludePaths)
	if err != nil || !clean {
		return clean, err
	}
	clean, err = g.diffCachedQuietExcept(ctx, excludePaths)
	if err != nil || !clean {
		return clean, err
	}
	hasUntracked, err := g.hasUntrackedExcept(ctx, excludePaths)
	if err != nil {
		return false, err
	}
	if hasUntracked {
		return false, nil
	}
	return true, nil
}

func (g Git) hasUntrackedExcept(ctx context.Context, ignoreUntracked []string) (bool, error) {
	out, err := g.gitOutputBytes(ctx, g.RepoRoot, g.env(), "git", "ls-files", "-o", "--exclude-standard", "-z")
	if err != nil {
		return false, err
	}
	paths := splitNullList(out)
	if len(paths) == 0 {
		return false, nil
	}
	ignoreSet := make(map[string]struct{}, len(ignoreUntracked))
	for _, path := range ignoreUntracked {
		path = filepath.ToSlash(strings.TrimSpace(path))
		if path == "" {
			continue
		}
		ignoreSet[path] = struct{}{}
	}
	for _, path := range paths {
		if _, ok := ignoreSet[path]; ok {
			continue
		}
		return true, nil
	}
	return false, nil
}

func (g Git) diffQuiet(ctx context.Context) (bool, error) {
	return g.runQuiet(ctx, "git", "diff", "--quiet")
}

func (g Git) diffCachedQuiet(ctx context.Context) (bool, error) {
	return g.runQuiet(ctx, "git", "diff", "--cached", "--quiet")
}

func (g Git) diffQuietExcept(ctx context.Context, excludePaths []string) (bool, error) {
	return g.runQuiet(ctx, "git", g.withExcludePathspecs([]string{"diff", "--quiet"}, excludePaths)...)
}

func (g Git) diffCachedQuietExcept(ctx context.Context, excludePaths []string) (bool, error) {
	return g.runQuiet(ctx, "git", g.withExcludePathspecs([]string{"diff", "--cached", "--quiet"}, excludePaths)...)
}

func (g Git) runQuiet(ctx context.Context, name string, args ...string) (bool, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = g.RepoRoot
	cmd.Env = g.env()
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 1 {
				return false, nil
			}
		}
		return false, err
	}
	return true, nil
}

func (g Git) withExcludePathspecs(args []string, excludePaths []string) []string {
	if len(excludePaths) == 0 {
		return args
	}
	out := append([]string{}, args...)
	out = append(out, "--", ".")
	for _, path := range excludePaths {
		path = filepath.ToSlash(strings.TrimSpace(path))
		if path == "" {
			continue
		}
		out = append(out, ":(exclude,literal)"+path)
	}
	return out
}

func (g Git) WorktreeAddDetach(ctx context.Context, path, ref string) error {
	return g.gitRun(ctx, g.RepoRoot, g.env(), "git", "-c", "core.autocrlf=false", "-c", "core.symlinks=true", "worktree", "add", "--detach", path, ref)
}

func (g Git) WorktreeRemoveForce(ctx context.Context, path string) error {
	return g.gitRun(ctx, g.RepoRoot, g.env(), "git", "worktree", "remove", "--force", path)
}

// RetainBase keeps the patch's immutable base reachable after its worktree is
// removed. The hashed namespace cannot replace user branches or moving refs.
func (g *Git) RetainBase(ctx context.Context, refName, oid string) error {
	const prefix = "refs/persona/bases/"
	key := strings.TrimPrefix(refName, prefix)
	if !strings.HasPrefix(refName, prefix) || len(key) != 64 || !isObjectID(key) {
		return fmt.Errorf("invalid retained base ref %q", refName)
	}
	if !isObjectID(oid) {
		return fmt.Errorf("invalid retained base object ID %q", oid)
	}
	emptyOID, err := g.emptyTreeOID(ctx, g.RepoRoot, g.GitDir)
	if err != nil {
		return err
	}
	if oid == emptyOID {
		// Computing an empty tree's OID does not write the object. A ref must
		// point at a real object so it remains valid during GC and restart.
		args := g.withDirArgs(g.RepoRoot, g.GitDir, "hash-object", "-w", "-t", "tree", "--stdin")
		if err := g.gitRun(ctx, g.RepoRoot, g.env(), "git", args...); err != nil {
			return fmt.Errorf("store empty base tree: %w", err)
		}
	} else {
		resolved, err := g.resolveBaseOID(ctx, g.RepoRoot, g.GitDir, oid)
		if err != nil {
			return err
		}
		if resolved != oid {
			return fmt.Errorf("retained base is not a canonical commit object ID: %q", oid)
		}
	}
	// Never follow symbolic refs, even if a concurrently replaced ref could
	// otherwise direct the update into a user's branch.
	args := g.withDirArgs(g.RepoRoot, g.GitDir, "symbolic-ref", "--quiet", refName)
	_, symErr := g.gitOutputBytes(ctx, g.RepoRoot, g.env(), "git", args...)
	var symExit *exec.ExitError
	if symErr == nil {
		return fmt.Errorf("retained base ref must not be symbolic: %s", refName)
	}
	if !errors.As(symErr, &symExit) || symExit.ExitCode() != 1 {
		return symErr
	}
	args = g.withDirArgs(g.RepoRoot, g.GitDir, "update-ref", "--no-deref", refName, oid, strings.Repeat("0", len(oid)))
	updateErr := g.gitRun(ctx, g.RepoRoot, g.env(), "git", args...)
	if updateErr == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// A concurrent creator with the same base is idempotent. A different
	// existing base is an error; its ref remains untouched.
	args = g.withDirArgs(g.RepoRoot, g.GitDir, "rev-parse", "--verify", "--quiet", "--end-of-options", refName)
	existing, err := g.gitOutputBytes(ctx, g.RepoRoot, g.env(), "git", args...)
	if err == nil && strings.TrimSpace(string(existing)) == oid {
		args = g.withDirArgs(g.RepoRoot, g.GitDir, "symbolic-ref", "--quiet", refName)
		_, err = g.gitOutputBytes(ctx, g.RepoRoot, g.env(), "git", args...)
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil
		}
	}
	return fmt.Errorf("retained base ref %s is immutable: %w", refName, updateErr)
}

// ResolveBase pins a session's base. A persisted BaseOID takes precedence over
// a moving ref; an unborn HEAD is represented by the repository's empty tree.
func (g *Git) ResolveBase(ctx context.Context, ref string) (string, error) {
	if g.BaseOID != "" {
		emptyOID, err := g.emptyTreeOID(ctx, g.RepoRoot, g.GitDir)
		if err != nil {
			return "", err
		}
		if g.BaseOID == emptyOID {
			return g.BaseOID, nil
		}
		if !isObjectID(g.BaseOID) {
			return "", fmt.Errorf("invalid base object ID %q", g.BaseOID)
		}
		ref = g.BaseOID
	}
	if ref == "" {
		ref = "HEAD"
	}
	oid, err := g.resolveBaseOID(ctx, g.RepoRoot, g.GitDir, ref)
	if err != nil {
		return "", err
	}
	g.BaseOID = oid
	return oid, nil
}

func isObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if !('0' <= ch && ch <= '9') && !('a' <= ch && ch <= 'f') {
			return false
		}
	}
	return true
}

func (g Git) emptyTreeOID(ctx context.Context, workTree, gitDir string) (string, error) {
	args := g.withDirArgs(workTree, gitDir, "hash-object", "-t", "tree", "--stdin")
	data, err := g.gitOutputBytes(ctx, workTree, g.envWith(workTree, gitDir), "git", args...)
	return strings.TrimSpace(string(data)), err
}

func (g Git) resolveBaseOID(ctx context.Context, workTree, gitDir, ref string) (string, error) {
	args := g.withDirArgs(workTree, gitDir, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	data, err := g.gitOutputBytes(ctx, workTree, g.envWith(workTree, gitDir), "git", args...)
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if isObjectID(ref) && ctx.Err() == nil {
		emptyOID, emptyErr := g.emptyTreeOID(ctx, workTree, gitDir)
		if emptyErr != nil {
			return "", emptyErr
		}
		if ref == emptyOID {
			return emptyOID, nil
		}
	}
	if ref != "HEAD" || ctx.Err() != nil {
		return "", fmt.Errorf("resolve base %q: %w", ref, err)
	}
	// Only a symbolic HEAD whose exact branch ref does not exist is unborn.
	// A corrupt ref or missing commit must never silently become an empty base.
	args = g.withDirArgs(workTree, gitDir, "symbolic-ref", "--quiet", "HEAD")
	branch, branchErr := g.gitOutputBytes(ctx, workTree, g.envWith(workTree, gitDir), "git", args...)
	if branchErr != nil {
		return "", fmt.Errorf("resolve base HEAD: %w", err)
	}
	args = g.withDirArgs(workTree, gitDir, "show-ref", "--verify", "--quiet", strings.TrimSpace(string(branch)))
	checkErr := g.gitRun(ctx, workTree, g.envWith(workTree, gitDir), "git", args...)
	var exitErr *exec.ExitError
	if !errors.As(checkErr, &exitErr) || exitErr.ExitCode() != 1 {
		return "", fmt.Errorf("resolve base HEAD: %w", err)
	}
	return g.emptyTreeOID(ctx, workTree, gitDir)
}

func (g Git) ApplyPatch(ctx context.Context, mode model.ApplyMode, workTree, gitDir string, patchData []byte) error {
	return g.ApplyPatchReader(ctx, mode, workTree, gitDir, bytes.NewReader(patchData))
}

func (g Git) ApplyPatchReader(ctx context.Context, mode model.ApplyMode, workTree, gitDir string, patchReader io.Reader) error {
	args := g.withDirArgs(workTree, gitDir, "-c", "core.autocrlf=false", "-c", "core.filemode=true", "-c", "core.symlinks=true", "-c", "apply.ignoreWhitespace=no", "apply", "--whitespace=nowarn")
	if mode == model.ApplyReject {
		args = append(args, "--reject")
	}
	args = append(args, "-")
	return g.gitRunWithReader(ctx, workTree, g.envWith(workTree, gitDir), patchReader, "git", args...)
}

func (g Git) DiffHeadBinary(ctx context.Context, workTree, gitDir string, excludePaths []string) ([]byte, error) {
	var out bytes.Buffer
	if err := g.DiffHeadBinaryTo(ctx, workTree, gitDir, excludePaths, &out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (g Git) DiffHeadBinaryTo(ctx context.Context, workTree, gitDir string, excludePaths []string, out io.Writer) error {
	index, err := g.exportEnv(ctx, workTree, gitDir)
	if err != nil {
		return err
	}
	defer index.cleanup()
	paths := make([]string, 0, len(index.paths))
	for _, path := range index.paths {
		excluded := false
		for _, exclude := range excludePaths {
			exclude = filepath.ToSlash(exclude)
			if exclude != "" && (path == exclude || strings.HasPrefix(path, strings.TrimSuffix(exclude, "/")+"/")) {
				excluded = true
				break
			}
		}
		if !excluded {
			paths = append(paths, path)
		}
	}
	if err := g.checkStorageAttributes(ctx, workTree, gitDir, paths, index); err != nil {
		return err
	}
	args := g.withDirArgs(workTree, gitDir, append(storageDiffArgs(), "-M", index.baseOID)...)
	if len(excludePaths) > 0 {
		args = append(args, "--", ".")
		for _, path := range excludePaths {
			if path == "" {
				continue
			}
			args = append(args, ":(exclude,literal)"+filepath.ToSlash(path))
		}
	}
	return g.gitDiffToWriter(ctx, workTree, index.env, out, "git", args...)
}

func (g Git) ListUntracked(ctx context.Context, workTree, gitDir string) ([]string, error) {
	index, err := g.exportEnv(ctx, workTree, gitDir)
	if err != nil {
		return nil, err
	}
	defer index.cleanup()
	args := g.withDirArgs(workTree, gitDir, "ls-files", "-o", "--exclude-standard", "-z")
	out, err := g.gitOutputBytes(ctx, workTree, index.env, "git", args...)
	if err != nil {
		return nil, err
	}
	return splitNullList(out), nil
}

func (g Git) DiffNewFileNoIndex(ctx context.Context, workTree, gitDir, relPath string) ([]byte, error) {
	var out bytes.Buffer
	if err := g.DiffNewFileNoIndexTo(ctx, workTree, gitDir, relPath, &out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (g Git) DiffNewFileNoIndexTo(ctx context.Context, workTree, gitDir, relPath string, out io.Writer) error {
	if err := g.checkStorageAttributes(ctx, workTree, gitDir, []string{relPath}, nil); err != nil {
		return err
	}
	args := g.withDirArgs(workTree, gitDir, append(storageDiffArgs(), "--no-index", "--", "/dev/null", relPath)...)
	return g.gitDiffToWriter(ctx, workTree, g.envWith(workTree, gitDir), out, "git", args...)
}

// Storage diffs must be applicable regardless of display-oriented user config.
func storageDiffArgs() []string {
	return []string{
		"-c", "core.quotepath=false", "-c", "core.autocrlf=false",
		"-c", "core.filemode=true", "-c", "core.symlinks=true", "-c", "core.ignorestat=false",
		"-c", "core.fsmonitor=false", "-c", "diff.mnemonicPrefix=false",
		"-c", "diff.suppressBlankEmpty=false",
		"diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv",
		"--no-color", "--no-relative", "--src-prefix=a/", "--dst-prefix=b/", "--unified=3",
		"--ignore-submodules=none", "--submodule=short",
	}
}

// exportEnv uses an index rooted in the immutable base, never the mutable
// checkout's index. Entries staged by the user are added only when absent from
// that base so unborn and newly staged files remain tracked for serialization.
type exportIndex struct {
	baseOID     string
	env         []string
	cleanup     func()
	paths       []string
	baseObjects map[string]string
}

func (g Git) exportEnv(ctx context.Context, workTree, gitDir string) (*exportIndex, error) {
	baseOID := g.BaseOID
	if baseOID == "" {
		var err error
		baseOID, err = g.resolveBaseOID(ctx, workTree, gitDir, "HEAD")
		if err != nil {
			return nil, err
		}
	}
	indexDir, err := os.MkdirTemp("", "persona-index-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(indexDir) }
	env := append(g.envWith(workTree, gitDir), "GIT_INDEX_FILE="+filepath.Join(indexDir, "index"))
	args := g.withDirArgs(workTree, gitDir, "-c", "core.splitIndex=false", "-c", "core.ignorestat=false", "-c", "core.fsmonitor=false", "read-tree", baseOID)
	if err := g.gitRun(ctx, workTree, env, "git", args...); err != nil {
		cleanup()
		return nil, err
	}
	args = g.withDirArgs(workTree, gitDir, "ls-files", "--stage", "-z")
	basePaths, err := g.gitOutputBytes(ctx, workTree, env, "git", args...)
	if err != nil {
		cleanup()
		return nil, err
	}
	paths := make(map[string]bool)
	baseObjects := make(map[string]string)
	for _, entry := range splitNullList(basePaths) {
		meta, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if ok && len(fields) == 3 {
			paths[path] = true
			baseObjects[path] = fields[1]
		}
	}
	args = g.withDirArgs(workTree, gitDir, "ls-files", "--stage", "-z")
	staged, err := g.gitOutputBytes(ctx, workTree, g.envWith(workTree, gitDir), "git", args...)
	if err != nil {
		cleanup()
		return nil, err
	}
	var extra bytes.Buffer
	for _, entry := range splitNullList(staged) {
		meta, path, ok := strings.Cut(entry, "\t")
		if ok && strings.HasSuffix(meta, " 0") && !paths[path] {
			// Only import additions that exist in this view. An absent source
			// index entry can otherwise replace a pinned parent/child entry
			// through update-index's directory/file conflict resolution.
			info, err := os.Lstat(filepath.Join(workTree, filepath.FromSlash(path)))
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
				continue
			}
			if err != nil {
				cleanup()
				return nil, err
			}
			if info.IsDir() && !strings.HasPrefix(meta, "160000 ") {
				continue
			}
			// Lstat still follows symlinks in parent components. Do not let
			// such a source-only child replace a pinned symlink in the index.
			blockedParent := false
			for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
				if !paths[parent] {
					continue
				}
				info, err := os.Lstat(filepath.Join(workTree, filepath.FromSlash(parent)))
				if err != nil || !info.IsDir() {
					blockedParent = true
					break
				}
			}
			if blockedParent {
				continue
			}
			extra.WriteString(entry)
			extra.WriteByte(0)
			paths[path] = true
		}
	}
	if extra.Len() != 0 {
		args = g.withDirArgs(workTree, gitDir, "-c", "core.splitIndex=false", "update-index", "-z", "--index-info")
		if err := g.gitRunWithReader(ctx, workTree, env, &extra, "git", args...); err != nil {
			cleanup()
			return nil, err
		}
	}
	allPaths := make([]string, 0, len(paths))
	for path := range paths {
		allPaths = append(allPaths, path)
	}
	return &exportIndex{baseOID: baseOID, env: env, cleanup: cleanup, paths: allPaths, baseObjects: baseObjects}, nil
}

// Git's clean conversions are distinct from display-only textconv. Reject
// changed files that use them rather than silently save normalized bytes.
// Unchanged raw blobs are safe even when a conversion attribute is configured.
func (g Git) checkStorageAttributes(ctx context.Context, workTree, gitDir string, paths []string, index *exportIndex) error {
	if len(paths) == 0 {
		return nil
	}
	var input bytes.Buffer
	for _, path := range paths {
		input.WriteString(path)
		input.WriteByte(0)
	}
	args := g.withDirArgs(workTree, gitDir, "check-attr", "-z", "--stdin", "text", "eol", "working-tree-encoding", "filter", "ident")
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workTree
	cmd.Env = g.envWith(workTree, gitDir)
	if index != nil {
		// check-attr falls back to the index when a .gitattributes file is
		// absent. Inspect exactly the same attributes that diff will use.
		cmd.Env = index.env
	}
	cmd.Stdin = &input
	var stderr boundedOutput
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("check content transformation attributes: %w: %s", err, truncateOutput(stderr.String()))
	}
	fields := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	checked := make(map[string]bool)
	var canonicalChanges map[string]bool
	for i := 0; i+2 < len(fields); i += 3 {
		path, attr, value := fields[i], fields[i+1], fields[i+2]
		if value == "unspecified" || value == "unset" || checked[path] {
			continue
		}
		checked[path] = true
		info, err := os.Lstat(filepath.Join(workTree, filepath.FromSlash(path)))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue // Deletions do not pass through content filters.
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue // Git never filters symlink targets or submodules.
		}
		if index != nil && index.baseObjects[path] != "" {
			oid := index.baseObjects[path]
			same, err := rawBlobMatches(ctx, filepath.Join(workTree, filepath.FromSlash(path)), info.Size(), oid)
			if err != nil {
				return err
			}
			if same {
				// An attribute may have changed since the commit: unchanged
				// raw bytes can still be normalized into a different diff.
				if canonicalChanges == nil {
					args := g.withDirArgs(workTree, gitDir, append(storageDiffArgs(), "--name-only", "-z", "--no-renames", index.baseOID)...)
					var diff bytes.Buffer
					if err := g.gitDiffToWriter(ctx, workTree, index.env, &diff, "git", args...); err != nil {
						return err
					}
					canonicalChanges = make(map[string]bool)
					for _, changed := range splitNullList(diff.Bytes()) {
						canonicalChanges[changed] = true
					}
				}
				if !canonicalChanges[path] {
					continue
				}
			}
		}
		return fmt.Errorf("cannot safely export %q: content transformation attribute %s=%s is unsupported", path, attr, value)
	}
	return nil
}

func rawBlobMatches(ctx context.Context, path string, size int64, oid string) (bool, error) {
	var digest hash.Hash = sha1.New()
	if len(oid) == 64 {
		digest = sha256.New()
	}
	_, _ = fmt.Fprintf(digest, "blob %d\x00", size)
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n, err := file.Read(buffer)
		if n > 0 {
			_, _ = digest.Write(buffer[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, err
		}
	}
	return fmt.Sprintf("%x", digest.Sum(nil)) == oid, nil
}

func (g Git) ListIgnoredCandidates(ctx context.Context, workTree, gitDir string, maxN int) ([]string, error) {
	args := g.withDirArgs(workTree, gitDir, "status", "--porcelain=v1", "-z", "--ignored=matching")
	out, err := g.gitOutputBytes(ctx, workTree, g.envWith(workTree, gitDir), "git", args...)
	if err != nil {
		return nil, err
	}
	entries := splitNullList(out)
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry, "!! ") {
			path := strings.TrimPrefix(entry, "!! ")
			if path == "" {
				continue
			}
			if strings.HasSuffix(path, "/") {
				path = strings.TrimSuffix(path, "/")
			}
			result = append(result, path)
			if maxN > 0 && len(result) > maxN {
				break
			}
		}
	}
	return result, nil
}

func (g Git) withDirArgs(workTree, gitDir string, args ...string) []string {
	out := make([]string, 0, len(args)+4)
	if gitDir != "" {
		out = append(out, "--git-dir", gitDir)
	}
	if workTree != "" {
		out = append(out, "--work-tree", workTree)
	}
	out = append(out, args...)
	return out
}

func splitNullList(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	parts := bytes.Split(data, []byte{0})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		out = append(out, string(part))
	}
	return out
}

func (g Git) env() []string {
	return g.envWith(g.RepoRoot, g.GitDir)
}

func (g Git) envWith(workTree, gitDir string) []string {
	base := FilterGitEnv(os.Environ())
	filtered := base[:0]
	for _, item := range base {
		if idx := strings.IndexByte(item, '='); idx > 0 {
			switch item[:idx] {
			case "LANG", "LC_ALL", "LC_MESSAGES":
				continue
			}
		}
		filtered = append(filtered, item)
	}
	base = filtered
	base = append(base, "LANG=C", "LC_ALL=C")
	// Persona's Linux view is case-sensitive even when this checkout was copied
	// from a case-insensitive filesystem. Discovery must not hide distinct files.
	base = append(base, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.ignorecase", "GIT_CONFIG_VALUE_0=false")
	if workTree != "" {
		base = append(base, "GIT_WORK_TREE="+workTree)
	}
	if gitDir != "" {
		base = append(base, "GIT_DIR="+gitDir)
	}
	return base
}

func FilterGitEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, item := range env {
		if idx := strings.IndexByte(item, '='); idx > 0 && strings.HasPrefix(item[:idx], "GIT_") {
			continue
		}
		out = append(out, item)
	}
	return out
}

// maxErrOutput is the maximum number of bytes from command output included
// in error messages.  Anything beyond this limit is truncated with a hint.
const maxErrOutput = 512

// boundedOutput limits allocation while still consuming the child's output.
// The extra byte lets truncateOutput report that output was truncated.
type boundedOutput struct {
	buffer        bytes.Buffer
	last          string
	alreadyExists bool
}

func (b *boundedOutput) Len() int       { return b.buffer.Len() }
func (b *boundedOutput) String() string { return b.buffer.String() }
func (b *boundedOutput) Bytes() []byte  { return b.buffer.Bytes() }

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	const existsMessage = "already exists in working directory"
	if !b.alreadyExists {
		prefix := p[:min(len(p), len(existsMessage)-1)]
		b.alreadyExists = bytes.Contains(p, []byte(existsMessage)) || strings.Contains(b.last+string(prefix), existsMessage)
		if len(p) >= len(existsMessage)-1 {
			b.last = string(p[len(p)-(len(existsMessage)-1):])
		} else {
			b.last += string(p)
			if len(b.last) >= len(existsMessage) {
				b.last = b.last[len(b.last)-(len(existsMessage)-1):]
			}
		}
	}
	if remaining := maxErrOutput + 1 - b.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buffer.Write(p)
	}
	return n, nil
}

// truncateOutput clips msg to at most maxErrOutput bytes, appending a
// truncation marker when the original was longer.
func truncateOutput(msg string) string {
	if len(msg) <= maxErrOutput {
		return msg
	}
	return msg[:maxErrOutput] + "... (truncated)"
}

func (g Git) gitRun(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	if g.Verbose {
		fmt.Fprintf(os.Stderr, "[git] %s %s\n", name, strings.Join(args, " "))
	}
	if g.Verbose {
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	var out boundedOutput
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("%w: %s", err, truncateOutput(out.String()))
	}
	return nil
}

func (g Git) gitRunWithReader(ctx context.Context, dir string, env []string, input io.Reader, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = input
	if g.Verbose {
		fmt.Fprintf(os.Stderr, "[git] %s %s\n", name, strings.Join(args, " "))
	}
	var out boundedOutput
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if g.Verbose && out.Len() > 0 {
		_, _ = os.Stderr.Write(out.Bytes())
	}
	if err != nil {
		wrapped := fmt.Errorf("%w: %s", err, truncateOutput(out.String()))
		if out.alreadyExists {
			return alreadyExistsApplyError{error: wrapped}
		}
		return wrapped
	}
	return nil
}

func (g Git) gitOutputBytes(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stderr boundedOutput
	cmd.Stderr = &stderr
	if g.Verbose {
		fmt.Fprintf(os.Stderr, "[git] %s %s\n", name, strings.Join(args, " "))
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, truncateOutput(stderr.String()))
	}
	return out, nil
}

func (g Git) gitDiffOutputBytes(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	if err := g.gitDiffToWriter(ctx, dir, env, &out, name, args...); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (g Git) gitDiffToWriter(ctx context.Context, dir string, env []string, out io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = out
	var stderr boundedOutput
	cmd.Stderr = &stderr
	if g.Verbose {
		fmt.Fprintf(os.Stderr, "[git] %s %s\n", name, strings.Join(args, " "))
	}
	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("%w: %s", err, truncateOutput(stderr.String()))
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stderr boundedOutput
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, truncateOutput(stderr.String()))
	}
	return string(bytes.TrimSpace(out)), nil
}
