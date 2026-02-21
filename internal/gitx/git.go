package gitx

import (
	"bytes"
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

func DetectRepo(cwd string) (string, string, error) {
	env := filterEnv(os.Environ(), "GIT_WORK_TREE", "GIT_DIR")
	repoRoot, err := gitOutput(cwd, env, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", err
	}
	gitDir, err := gitOutput(cwd, env, "git", "rev-parse", "--git-dir")
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

func (g Git) IsClean() (bool, error) {
	return g.IsCleanExceptUntracked(nil)
}

func (g Git) IsCleanExceptUntracked(ignoreUntracked []string) (bool, error) {
	clean, err := g.diffQuiet()
	if err != nil || !clean {
		return clean, err
	}
	clean, err = g.diffCachedQuiet()
	if err != nil || !clean {
		return clean, err
	}
	hasUntracked, err := g.hasUntrackedExcept(ignoreUntracked)
	if err != nil {
		return false, err
	}
	if hasUntracked {
		return false, nil
	}
	return true, nil
}

func (g Git) hasUntrackedExcept(ignoreUntracked []string) (bool, error) {
	out, err := g.gitOutputBytes(g.RepoRoot, g.env(), "git", "ls-files", "-o", "--exclude-standard", "-z")
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

func (g Git) diffQuiet() (bool, error) {
	return g.runQuiet("git", "diff", "--quiet")
}

func (g Git) diffCachedQuiet() (bool, error) {
	return g.runQuiet("git", "diff", "--cached", "--quiet")
}

func (g Git) runQuiet(name string, args ...string) (bool, error) {
	cmd := exec.Command(name, args...)
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

func (g Git) WorktreeAddDetach(path, ref string) error {
	return g.gitRun(g.RepoRoot, g.env(), "git", "worktree", "add", "--detach", path, ref)
}

func (g Git) WorktreeRemoveForce(path string) error {
	return g.gitRun(g.RepoRoot, g.env(), "git", "worktree", "remove", "--force", path)
}

func (g Git) ApplyPatch(mode model.ApplyMode, workTree, gitDir string, patchData []byte) error {
	args := g.withDirArgs(workTree, gitDir, "apply", "--whitespace=nowarn")
	if mode == model.ApplyReject {
		args = append(args, "--reject")
	}
	args = append(args, "-")
	return g.gitRunWithInput(workTree, g.envWith(workTree, gitDir), patchData, "git", args...)
}

func (g Git) DiffHeadBinary(workTree, gitDir string) ([]byte, error) {
	hasHead, err := g.hasHead(workTree, gitDir)
	if err != nil {
		return nil, err
	}
	if !hasHead {
		return nil, nil
	}
	args := g.withDirArgs(workTree, gitDir, "-c", "core.quotepath=false", "diff", "--binary", "--full-index", "-M", "--no-ext-diff", "HEAD")
	return g.gitDiffOutputBytes(workTree, g.envWith(workTree, gitDir), "git", args...)
}

func (g Git) ListUntracked(workTree, gitDir string) ([]string, error) {
	args := g.withDirArgs(workTree, gitDir, "ls-files", "-o", "--exclude-standard", "-z")
	out, err := g.gitOutputBytes(workTree, g.envWith(workTree, gitDir), "git", args...)
	if err != nil {
		return nil, err
	}
	return splitNullList(out), nil
}

func (g Git) DiffNewFileNoIndex(workTree, gitDir, relPath string) ([]byte, error) {
	args := g.withDirArgs(workTree, gitDir, "-c", "core.quotepath=false", "diff", "--no-index", "--binary", "--", "/dev/null", relPath)
	return g.gitDiffOutputBytes(workTree, g.envWith(workTree, gitDir), "git", args...)
}

func (g Git) ListIgnoredCandidates(workTree, gitDir string, maxN int) ([]string, error) {
	args := g.withDirArgs(workTree, gitDir, "status", "--porcelain=v1", "-z", "--ignored=matching")
	out, err := g.gitOutputBytes(workTree, g.envWith(workTree, gitDir), "git", args...)
	if err != nil {
		return nil, err
	}
	entries := splitNullList(out)
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry, "!! ") {
			path := strings.TrimSpace(strings.TrimPrefix(entry, "!! "))
			if path == "" {
				continue
			}
			if strings.HasSuffix(path, "/") {
				path = strings.TrimSuffix(path, "/")
			}
			result = append(result, path)
			if maxN > 0 && len(result) >= maxN {
				break
			}
		}
	}
	return result, nil
}

func (g Git) hasHead(workTree, gitDir string) (bool, error) {
	args := g.withDirArgs(workTree, gitDir, "rev-parse", "--verify", "HEAD")
	cmd := exec.Command("git", args...)
	cmd.Dir = g.RepoRoot
	cmd.Env = g.envWith(workTree, gitDir)
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 128 {
				return false, nil
			}
		}
		return false, err
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
	base := filterEnv(os.Environ(), "GIT_WORK_TREE", "GIT_DIR")
	if workTree != "" {
		base = append(base, "GIT_WORK_TREE="+workTree)
	}
	if gitDir != "" {
		base = append(base, "GIT_DIR="+gitDir)
	}
	return base
}

func (g Git) gitRun(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
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
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

func filterEnv(env []string, keys ...string) []string {
	if len(keys) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, item := range env {
		keep := true
		for _, key := range keys {
			if strings.HasPrefix(item, key+"=") {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, item)
		}
	}
	return out
}

func (g Git) gitRunWithInput(dir string, env []string, input []byte, name string, args ...string) error {
	cmd := exec.Command(name, args...)
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
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

func (g Git) gitOutputBytes(dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	if g.Verbose {
		fmt.Fprintf(os.Stderr, "[git] %s %s\n", name, strings.Join(args, " "))
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, string(out))
	}
	return out, nil
}

func (g Git) gitDiffOutputBytes(dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	if g.Verbose {
		fmt.Fprintf(os.Stderr, "[git] %s %s\n", name, strings.Join(args, " "))
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return out, nil
		}
		return nil, fmt.Errorf("%w: %s", err, string(out))
	}
	return out, nil
}

func gitOutput(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, string(out))
	}
	return string(bytes.TrimSpace(out)), nil
}
