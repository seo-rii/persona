package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"persona/internal/model"
)

type Git struct {
	RepoRoot string
	GitDir   string
	Verbose  bool
}

// Compile-time check that Git satisfies model.GitOps.
var _ model.GitOps = (*Git)(nil)

// RepoRootPath returns the repository root directory.
func (g *Git) RepoRootPath() string { return g.RepoRoot }

// GitDirPath returns the .git directory path.
func (g *Git) GitDirPath() string { return g.GitDir }

func DetectRepo(ctx context.Context, cwd string) (string, string, error) {
	env := filterGitEnv(os.Environ())
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
	return g.gitRun(ctx, g.RepoRoot, g.env(), "git", "worktree", "add", "--detach", path, ref)
}

func (g Git) WorktreeRemoveForce(ctx context.Context, path string) error {
	return g.gitRun(ctx, g.RepoRoot, g.env(), "git", "worktree", "remove", "--force", path)
}

func (g Git) ApplyPatch(ctx context.Context, mode model.ApplyMode, workTree, gitDir string, patchData []byte) error {
	args := g.withDirArgs(workTree, gitDir, "apply", "--whitespace=nowarn")
	if mode == model.ApplyReject {
		args = append(args, "--reject")
	}
	args = append(args, "-")
	return g.gitRunWithInput(ctx, workTree, g.envWith(workTree, gitDir), patchData, "git", args...)
}

func (g Git) DiffHeadBinary(ctx context.Context, workTree, gitDir string, excludePaths []string) ([]byte, error) {
	hasHead, err := g.hasHead(ctx, workTree, gitDir)
	if err != nil {
		return nil, err
	}
	if !hasHead {
		return nil, nil
	}
	args := g.withDirArgs(workTree, gitDir, "-c", "core.quotepath=false", "diff", "--binary", "--full-index", "-M", "--no-ext-diff", "HEAD")
	if len(excludePaths) > 0 {
		args = append(args, "--", ".")
		for _, path := range excludePaths {
			if path == "" {
				continue
			}
			args = append(args, ":(exclude,literal)"+filepath.ToSlash(path))
		}
	}
	return g.gitDiffOutputBytes(ctx, workTree, g.envWith(workTree, gitDir), "git", args...)
}

func (g Git) ListUntracked(ctx context.Context, workTree, gitDir string) ([]string, error) {
	args := g.withDirArgs(workTree, gitDir, "ls-files", "-o", "--exclude-standard", "-z")
	out, err := g.gitOutputBytes(ctx, workTree, g.envWith(workTree, gitDir), "git", args...)
	if err != nil {
		return nil, err
	}
	return splitNullList(out), nil
}

func (g Git) DiffNewFileNoIndex(ctx context.Context, workTree, gitDir, relPath string) ([]byte, error) {
	args := g.withDirArgs(workTree, gitDir, "-c", "core.quotepath=false", "diff", "--no-index", "--binary", "--", "/dev/null", relPath)
	return g.gitDiffOutputBytes(ctx, workTree, g.envWith(workTree, gitDir), "git", args...)
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

func (g Git) hasHead(ctx context.Context, workTree, gitDir string) (bool, error) {
	args := g.withDirArgs(workTree, gitDir, "show-ref", "--head", "--quiet")
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workTree
	cmd.Env = g.envWith(workTree, gitDir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 1 {
				return false, nil
			}
		}
		return false, fmt.Errorf("%w: %s", err, truncateOutput(stderr.String()))
	}
	return true, nil
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
	base := filterGitEnv(os.Environ())
	if workTree != "" {
		base = append(base, "GIT_WORK_TREE="+workTree)
	}
	if gitDir != "" {
		base = append(base, "GIT_DIR="+gitDir)
	}
	return base
}

func filterGitEnv(env []string) []string {
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
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, truncateOutput(string(out)))
	}
	return nil
}

func filterEnv(env []string, keys ...string) []string {
	if len(keys) == 0 {
		return env
	}
	exclude := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		exclude[key] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, item := range env {
		if idx := strings.IndexByte(item, '='); idx > 0 {
			if _, ok := exclude[item[:idx]]; ok {
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func (g Git) gitRunWithInput(ctx context.Context, dir string, env []string, input []byte, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(input)
	if g.Verbose {
		fmt.Fprintf(os.Stderr, "[git] %s %s\n", name, strings.Join(args, " "))
	}
	out, err := cmd.CombinedOutput()
	if g.Verbose && len(out) > 0 {
		_, _ = os.Stderr.Write(out)
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, truncateOutput(string(out)))
	}
	return nil
}

func (g Git) gitOutputBytes(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stderr bytes.Buffer
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
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if g.Verbose {
		fmt.Fprintf(os.Stderr, "[git] %s %s\n", name, strings.Join(args, " "))
	}
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return out, nil
		}
		return nil, fmt.Errorf("%w: %s", err, truncateOutput(stderr.String()))
	}
	return out, nil
}

func gitOutput(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, truncateOutput(stderr.String()))
	}
	return string(bytes.TrimSpace(out)), nil
}
