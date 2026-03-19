package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"persona/internal/gitx"
	"persona/internal/model"
	"persona/internal/patchio"
	"persona/internal/session"
	"persona/internal/testutil"
)

func TestIsSubpathInsideWithDotDotPrefixName(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "..cache", "state.patch")

	ok, rel := isSubpath(root, path)
	expected := filepath.Join("..cache", "state.patch")
	if !ok {
		t.Fatalf("expected path to be treated as subpath")
	}
	if rel != expected {
		t.Fatalf("expected rel %q, got %q", expected, rel)
	}
}

func TestIsSubpathOutsideParent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "..", "state.patch")

	ok, rel := isSubpath(root, path)
	if ok {
		t.Fatalf("expected path outside root, got rel %q", rel)
	}
}

func TestIsSubpathRootItself(t *testing.T) {
	root := t.TempDir()

	ok, rel := isSubpath(root, root)
	if !ok {
		t.Fatalf("expected root to be subpath")
	}
	if rel != "." {
		t.Fatalf("expected rel '.', got %q", rel)
	}
}

func TestNewRootCmdDoesNotExposeIgnoredScopeFlag(t *testing.T) {
	cmd := newRootCmd()
	flag := cmd.Flags().Lookup("ignored-scope")
	if flag != nil {
		t.Fatal("expected ignored-scope flag to be removed")
	}
}

type buildOptionsInput struct {
	patchPath      string
	patchDir       string
	printPatchPath bool
	baseMode       string
	baseRef        string
	allowDirty     bool
	ignoredMode    string
	ignoredMax     int
	applyMode      string
	keepSession    string
	verbose        bool
	args           []string
}

type ignoredListGitOps struct {
	ignored []string
	err     error
}

type exportGitOps struct {
	tracked    []byte
	untracked  []string
	ignored    []string
	ignoredErr error
	diffByPath map[string][]byte
	errByPath  map[string]error
}

func (g ignoredListGitOps) RepoRootPath() string { return "" }

func (g ignoredListGitOps) GitDirPath() string { return "" }

func (g ignoredListGitOps) IsCleanExceptUntracked(context.Context, []string) (bool, error) {
	panic("unexpected call")
}

func (g ignoredListGitOps) IsCleanExceptPaths(context.Context, []string) (bool, error) {
	panic("unexpected call")
}

func (g ignoredListGitOps) WorktreeAddDetach(context.Context, string, string) error {
	panic("unexpected call")
}

func (g ignoredListGitOps) WorktreeRemoveForce(context.Context, string) error {
	panic("unexpected call")
}

func (g ignoredListGitOps) ApplyPatch(context.Context, model.ApplyMode, string, string, []byte) error {
	panic("unexpected call")
}

func (g ignoredListGitOps) DiffHeadBinary(context.Context, string, string, []string) ([]byte, error) {
	panic("unexpected call")
}

func (g ignoredListGitOps) ListUntracked(context.Context, string, string) ([]string, error) {
	panic("unexpected call")
}

func (g ignoredListGitOps) DiffNewFileNoIndex(context.Context, string, string, string) ([]byte, error) {
	panic("unexpected call")
}

func (g ignoredListGitOps) ListIgnoredCandidates(_ context.Context, _ string, _ string, maxN int) ([]string, error) {
	if g.err != nil {
		return nil, g.err
	}
	if maxN > 0 && len(g.ignored) > maxN {
		limit := maxN + 1
		if limit > len(g.ignored) {
			limit = len(g.ignored)
		}
		return append([]string(nil), g.ignored[:limit]...), nil
	}
	return append([]string(nil), g.ignored...), nil
}

func (g exportGitOps) RepoRootPath() string { return "" }

func (g exportGitOps) GitDirPath() string { return "" }

func (g exportGitOps) IsCleanExceptUntracked(context.Context, []string) (bool, error) {
	panic("unexpected call")
}

func (g exportGitOps) IsCleanExceptPaths(context.Context, []string) (bool, error) {
	panic("unexpected call")
}

func (g exportGitOps) WorktreeAddDetach(context.Context, string, string) error {
	panic("unexpected call")
}

func (g exportGitOps) WorktreeRemoveForce(context.Context, string) error {
	panic("unexpected call")
}

func (g exportGitOps) ApplyPatch(context.Context, model.ApplyMode, string, string, []byte) error {
	panic("unexpected call")
}

func (g exportGitOps) DiffHeadBinary(context.Context, string, string, []string) ([]byte, error) {
	return g.tracked, nil
}

func (g exportGitOps) ListUntracked(context.Context, string, string) ([]string, error) {
	return g.untracked, nil
}

func (g exportGitOps) DiffNewFileNoIndex(_ context.Context, _ string, _ string, relPath string) ([]byte, error) {
	if err, ok := g.errByPath[relPath]; ok {
		return nil, err
	}
	return g.diffByPath[relPath], nil
}

func (g exportGitOps) ListIgnoredCandidates(_ context.Context, _ string, _ string, maxN int) ([]string, error) {
	if g.ignoredErr != nil {
		return nil, g.ignoredErr
	}
	if maxN > 0 && len(g.ignored) > maxN {
		limit := maxN + 1
		if limit > len(g.ignored) {
			limit = len(g.ignored)
		}
		return append([]string(nil), g.ignored[:limit]...), nil
	}
	return append([]string(nil), g.ignored...), nil
}

type recordingNSOps struct {
	bindCalls    []string
	maskCalls    []maskCall
	remountCalls []string
	bindErrs     map[string]error
	remountErrs  map[string]error
}

type maskCall struct {
	target    string
	kind      model.MaskKind
	emptyFile string
	emptyDir  string
}

type worktreeGitOps struct {
	addCalls    []string
	removeCalls []string
}

type repoCleanGitOps struct {
	excludeCalls [][]string
}

type applyRetryGitOps struct {
	applyCalls int
	applyErrs  []error
}

func (g *worktreeGitOps) RepoRootPath() string { return "/repo" }

func (g *worktreeGitOps) GitDirPath() string { return "/repo/.git" }

func (g *worktreeGitOps) IsCleanExceptUntracked(context.Context, []string) (bool, error) {
	panic("unexpected call")
}

func (g *worktreeGitOps) IsCleanExceptPaths(context.Context, []string) (bool, error) {
	panic("unexpected call")
}

func (g *worktreeGitOps) WorktreeAddDetach(_ context.Context, path, ref string) error {
	g.addCalls = append(g.addCalls, path+"@"+ref)
	return nil
}

func (g *worktreeGitOps) WorktreeRemoveForce(_ context.Context, path string) error {
	g.removeCalls = append(g.removeCalls, path)
	return nil
}

func (g *worktreeGitOps) ApplyPatch(context.Context, model.ApplyMode, string, string, []byte) error {
	panic("unexpected call")
}

func (g *worktreeGitOps) DiffHeadBinary(context.Context, string, string, []string) ([]byte, error) {
	panic("unexpected call")
}

func (g *worktreeGitOps) ListUntracked(context.Context, string, string) ([]string, error) {
	panic("unexpected call")
}

func (g *worktreeGitOps) DiffNewFileNoIndex(context.Context, string, string, string) ([]byte, error) {
	panic("unexpected call")
}

func (g *worktreeGitOps) ListIgnoredCandidates(context.Context, string, string, int) ([]string, error) {
	panic("unexpected call")
}

func (g *repoCleanGitOps) RepoRootPath() string { return "/repo" }

func (g *repoCleanGitOps) GitDirPath() string { return "/repo/.git" }

func (g *repoCleanGitOps) IsCleanExceptUntracked(context.Context, []string) (bool, error) {
	panic("unexpected call")
}

func (g *repoCleanGitOps) IsCleanExceptPaths(_ context.Context, excludePaths []string) (bool, error) {
	g.excludeCalls = append(g.excludeCalls, append([]string(nil), excludePaths...))
	return true, nil
}

func (g *repoCleanGitOps) WorktreeAddDetach(context.Context, string, string) error {
	panic("unexpected call")
}

func (g *repoCleanGitOps) WorktreeRemoveForce(context.Context, string) error {
	panic("unexpected call")
}

func (g *repoCleanGitOps) ApplyPatch(context.Context, model.ApplyMode, string, string, []byte) error {
	panic("unexpected call")
}

func (g *repoCleanGitOps) DiffHeadBinary(context.Context, string, string, []string) ([]byte, error) {
	panic("unexpected call")
}

func (g *repoCleanGitOps) ListUntracked(context.Context, string, string) ([]string, error) {
	panic("unexpected call")
}

func (g *repoCleanGitOps) DiffNewFileNoIndex(context.Context, string, string, string) ([]byte, error) {
	panic("unexpected call")
}

func (g *repoCleanGitOps) ListIgnoredCandidates(context.Context, string, string, int) ([]string, error) {
	panic("unexpected call")
}

func (g *applyRetryGitOps) RepoRootPath() string { return "" }

func (g *applyRetryGitOps) GitDirPath() string { return "" }

func (g *applyRetryGitOps) IsCleanExceptUntracked(context.Context, []string) (bool, error) {
	panic("unexpected call")
}

func (g *applyRetryGitOps) IsCleanExceptPaths(context.Context, []string) (bool, error) {
	panic("unexpected call")
}

func (g *applyRetryGitOps) WorktreeAddDetach(context.Context, string, string) error {
	panic("unexpected call")
}

func (g *applyRetryGitOps) WorktreeRemoveForce(context.Context, string) error {
	panic("unexpected call")
}

func (g *applyRetryGitOps) ApplyPatch(context.Context, model.ApplyMode, string, string, []byte) error {
	g.applyCalls++
	if len(g.applyErrs) >= g.applyCalls {
		return g.applyErrs[g.applyCalls-1]
	}
	return nil
}

func (g *applyRetryGitOps) DiffHeadBinary(context.Context, string, string, []string) ([]byte, error) {
	panic("unexpected call")
}

func (g *applyRetryGitOps) ListUntracked(context.Context, string, string) ([]string, error) {
	panic("unexpected call")
}

func (g *applyRetryGitOps) DiffNewFileNoIndex(context.Context, string, string, string) ([]byte, error) {
	panic("unexpected call")
}

func (g *applyRetryGitOps) ListIgnoredCandidates(context.Context, string, string, int) ([]string, error) {
	panic("unexpected call")
}

func (m *recordingNSOps) UnshareMountNS() error { return nil }

func (m *recordingNSOps) MakeMountsPrivate() error { return nil }

func (m *recordingNSOps) BindMount(_, dst string) error {
	m.bindCalls = append(m.bindCalls, dst)
	if err, ok := m.bindErrs[dst]; ok {
		return err
	}
	return nil
}

func (m *recordingNSOps) RemountRO(target string) error {
	m.remountCalls = append(m.remountCalls, target)
	if err, ok := m.remountErrs[target]; ok {
		return err
	}
	return nil
}

func (m *recordingNSOps) Umount(string) error { return nil }

func (m *recordingNSOps) MountOverlay(string, model.OverlayOpts) error { return nil }

func (m *recordingNSOps) MaskPath(target string, kind model.MaskKind, emptyFile, emptyDir string) error {
	m.maskCalls = append(m.maskCalls, maskCall{
		target:    target,
		kind:      kind,
		emptyFile: emptyFile,
		emptyDir:  emptyDir,
	})
	return nil
}

func defaultBuildOptionsInput() buildOptionsInput {
	return buildOptionsInput{
		patchPath:      "",
		patchDir:       "",
		printPatchPath: false,
		baseMode:       string(model.BaseRepo),
		baseRef:        "HEAD",
		allowDirty:     false,
		ignoredMode:    string(model.IgnoredTransparent),
		ignoredMax:     200,
		applyMode:      string(model.ApplyStrict),
		keepSession:    string(model.KeepOnFail),
		verbose:        false,
		args:           []string{"sh", "-c", "true"},
	}
}

func runBuildOptionsInput(in buildOptionsInput) (model.Options, error) {
	return buildOptions(
		in.patchPath, in.patchDir, in.printPatchPath,
		in.baseMode, in.baseRef, in.allowDirty,
		in.ignoredMode, in.ignoredMax,
		in.applyMode, in.keepSession, in.verbose,
		in.args,
	)
}

func TestBuildOptionsDefaults(t *testing.T) {
	in := defaultBuildOptionsInput()
	opts, err := runBuildOptionsInput(in)
	if err != nil {
		t.Fatalf("buildOptions error: %v", err)
	}
	if opts.PatchPath != "" || opts.PatchDir != "" || opts.PrintPatchPath {
		t.Fatalf("unexpected patch defaults: %+v", opts)
	}
	if opts.BaseMode != model.BaseRepo || opts.BaseRef != "HEAD" || opts.AllowDirty {
		t.Fatalf("unexpected base defaults: %+v", opts)
	}
	if opts.IgnoredMode != model.IgnoredTransparent || opts.IgnoredMax != 200 {
		t.Fatalf("unexpected ignored defaults: %+v", opts)
	}
	if opts.ApplyMode != model.ApplyStrict || opts.KeepSession != model.KeepOnFail || opts.Verbose {
		t.Fatalf("unexpected apply/session defaults: %+v", opts)
	}
	if strings.Join(opts.Command, " ") != "sh -c true" {
		t.Fatalf("unexpected command: %v", opts.Command)
	}
}

func TestBuildOptionsValidValues(t *testing.T) {
	in := defaultBuildOptionsInput()
	in.patchPath = "state.patch"
	in.patchDir = "patches"
	in.printPatchPath = true
	in.baseMode = string(model.BaseWorktree)
	in.baseRef = "HEAD~1"
	in.allowDirty = true
	in.ignoredMode = string(model.IgnoredMasked)
	in.ignoredMax = 0
	in.applyMode = string(model.ApplyReject)
	in.keepSession = string(model.KeepAlways)
	in.verbose = true
	in.args = []string{"echo", "ok"}

	opts, err := runBuildOptionsInput(in)
	if err != nil {
		t.Fatalf("buildOptions error: %v", err)
	}
	if opts.BaseMode != model.BaseWorktree || opts.BaseRef != "HEAD~1" {
		t.Fatalf("unexpected base options: %+v", opts)
	}
	if opts.IgnoredMode != model.IgnoredMasked || opts.IgnoredMax != 0 {
		t.Fatalf("unexpected ignored options: %+v", opts)
	}
	if opts.ApplyMode != model.ApplyReject || opts.KeepSession != model.KeepAlways || !opts.Verbose {
		t.Fatalf("unexpected apply/session options: %+v", opts)
	}
	if strings.Join(opts.Command, " ") != "echo ok" {
		t.Fatalf("unexpected command: %v", opts.Command)
	}
}

func TestBuildOptionsInvalidValues(t *testing.T) {
	cases := []struct {
		name   string
		want   string
		mutate func(*buildOptionsInput)
	}{
		{
			name: "invalid base mode",
			want: "invalid base-mode",
			mutate: func(in *buildOptionsInput) {
				in.baseMode = "bad"
			},
		},
		{
			name: "invalid ignored mode",
			want: "invalid ignored-mode",
			mutate: func(in *buildOptionsInput) {
				in.ignoredMode = "bad"
			},
		},
		{
			name: "negative ignored max",
			want: "ignored-max must be >= 0",
			mutate: func(in *buildOptionsInput) {
				in.ignoredMax = -1
			},
		},
		{
			name: "invalid apply mode",
			want: "invalid apply-mode",
			mutate: func(in *buildOptionsInput) {
				in.applyMode = "bad"
			},
		},
		{
			name: "invalid keep session",
			want: "invalid keep-session",
			mutate: func(in *buildOptionsInput) {
				in.keepSession = "bad"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := defaultBuildOptionsInput()
			tc.mutate(&in)
			_, err := runBuildOptionsInput(in)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestShouldRemoveSession(t *testing.T) {
	testErr := fmt.Errorf("test error")
	cases := []struct {
		name string
		opts model.Options
		err  error
		want bool
	}{
		{
			name: "keep always success",
			opts: model.Options{KeepSession: model.KeepAlways},
			err:  nil,
			want: false,
		},
		{
			name: "keep always failure",
			opts: model.Options{KeepSession: model.KeepAlways},
			err:  model.Wrap(model.ExitApply, "test", testErr),
			want: false,
		},
		{
			name: "keep never success",
			opts: model.Options{KeepSession: model.KeepNever},
			err:  nil,
			want: true,
		},
		{
			name: "keep never failure",
			opts: model.Options{KeepSession: model.KeepNever},
			err:  model.Wrap(model.ExitApply, "test", testErr),
			want: true,
		},
		{
			name: "keep on fail success",
			opts: model.Options{KeepSession: model.KeepOnFail},
			err:  nil,
			want: true,
		},
		{
			name: "keep on fail failure",
			opts: model.Options{KeepSession: model.KeepOnFail},
			err:  model.Wrap(model.ExitApply, "test", testErr),
			want: false,
		},
		{
			name: "unknown policy defaults remove",
			opts: model.Options{KeepSession: model.KeepSessionPolicy("unknown")},
			err:  model.Wrap(model.ExitApply, "test", testErr),
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldRemoveSession(tc.err, tc.opts)
			if got != tc.want {
				t.Fatalf("expected %v got %v", tc.want, got)
			}
		})
	}
}

func TestShouldForceMountFail(t *testing.T) {
	prev, hadPrev := os.LookupEnv(forceMountFailEnv)
	t.Cleanup(func() {
		if hadPrev {
			if err := os.Setenv(forceMountFailEnv, prev); err != nil {
				t.Fatalf("restore env: %v", err)
			}
			return
		}
		if err := os.Unsetenv(forceMountFailEnv); err != nil {
			t.Fatalf("unset env: %v", err)
		}
	})

	if err := os.Unsetenv(forceMountFailEnv); err != nil {
		t.Fatalf("unset env: %v", err)
	}
	if shouldForceMountFail() {
		t.Fatalf("expected false when env is unset")
	}

	if err := os.Setenv(forceMountFailEnv, "0"); err != nil {
		t.Fatalf("set env: %v", err)
	}
	if shouldForceMountFail() {
		t.Fatalf("expected false when env is not 1")
	}

	if err := os.Setenv(forceMountFailEnv, "1"); err != nil {
		t.Fatalf("set env: %v", err)
	}
	if !shouldForceMountFail() {
		t.Fatalf("expected true when env is 1")
	}
}

func TestPrepareBaseWorktreeCleanupRespectsKeepSession(t *testing.T) {
	t.Run("keep always keeps worktree", func(t *testing.T) {
		g := &worktreeGitOps{}
		sess := &session.Session{BaseWT: filepath.Join(t.TempDir(), "basewt")}
		cleanup := &cleanupStack{}
		opts := model.Options{BaseMode: model.BaseWorktree, BaseRef: "HEAD", KeepSession: model.KeepAlways}
		var retErr error

		basePath, err := prepareBase(context.Background(), g, opts, sess, false, "", cleanup, &retErr)
		if err != nil {
			t.Fatalf("prepareBase error: %v", err)
		}
		if basePath != sess.BaseWT {
			t.Fatalf("expected basewt path, got %q", basePath)
		}
		if err := cleanup.Run(); err != nil {
			t.Fatalf("cleanup error: %v", err)
		}
		if len(g.removeCalls) != 0 {
			t.Fatalf("expected base worktree to be preserved, got removals %v", g.removeCalls)
		}
	})

	t.Run("keep never removes worktree", func(t *testing.T) {
		g := &worktreeGitOps{}
		sess := &session.Session{BaseWT: filepath.Join(t.TempDir(), "basewt")}
		cleanup := &cleanupStack{}
		opts := model.Options{BaseMode: model.BaseWorktree, BaseRef: "HEAD", KeepSession: model.KeepNever}
		var retErr error

		if _, err := prepareBase(context.Background(), g, opts, sess, false, "", cleanup, &retErr); err != nil {
			t.Fatalf("prepareBase error: %v", err)
		}
		if err := cleanup.Run(); err != nil {
			t.Fatalf("cleanup error: %v", err)
		}
		if len(g.removeCalls) != 1 || g.removeCalls[0] != sess.BaseWT {
			t.Fatalf("expected worktree removal, got %v", g.removeCalls)
		}
	})
}

func TestPrepareBaseRepoExcludesPatchAndLockPaths(t *testing.T) {
	g := &repoCleanGitOps{}
	sess := &session.Session{}
	cleanup := &cleanupStack{}
	opts := model.Options{BaseMode: model.BaseRepo, KeepSession: model.KeepOnFail}
	var retErr error

	basePath, err := prepareBase(context.Background(), g, opts, sess, true, "state.patch", cleanup, &retErr)
	if err != nil {
		t.Fatalf("prepareBase error: %v", err)
	}
	if basePath != "/repo" {
		t.Fatalf("expected repo base path, got %q", basePath)
	}
	if len(g.excludeCalls) != 1 {
		t.Fatalf("expected a single clean-check call, got %d", len(g.excludeCalls))
	}
	want := []string{"state.patch", "state.patch.lock"}
	if strings.Join(g.excludeCalls[0], ",") != strings.Join(want, ",") {
		t.Fatalf("expected excluded paths %v, got %v", want, g.excludeCalls[0])
	}
}

func copyDirTree(t *testing.T, src, dst string) {
	t.Helper()
	if err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	}); err != nil {
		t.Fatalf("copy dir tree: %v", err)
	}
}

func runGitWithDir(t *testing.T, workTree, gitDir string, args ...string) string {
	t.Helper()
	cmdArgs := []string{"--git-dir", gitDir, "--work-tree", workTree}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.Command("git", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v: %s", args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func TestPlanGitDirMountRepoKeepsGitOpsWorkingAfterRelocation(t *testing.T) {
	repo := testutil.InitRepo(t)
	_, gitDir, err := gitx.DetectRepo(context.Background(), repo)
	if err != nil {
		t.Fatalf("DetectRepo error: %v", err)
	}
	mountRoot := filepath.Join(t.TempDir(), "mnt", "gitdir")
	mountSrc, gitDirForOps, err := planGitDirMount(gitDir, mountRoot)
	if err != nil {
		t.Fatalf("planGitDirMount error: %v", err)
	}
	if mountSrc != gitDir {
		t.Fatalf("expected regular repo to mount gitdir directly, got %q", mountSrc)
	}
	copyRoot := t.TempDir()
	copyDirTree(t, mountSrc, copyRoot)
	relGitDir, err := filepath.Rel(mountRoot, gitDirForOps)
	if err != nil {
		t.Fatalf("Rel error: %v", err)
	}
	gotTop := runGitWithDir(t, repo, filepath.Join(copyRoot, relGitDir), "rev-parse", "--show-toplevel")
	if gotTop != repo {
		t.Fatalf("expected repo root %q got %q", repo, gotTop)
	}
}

func TestPlanGitDirMountLinkedWorktreeKeepsRelativeCommonDir(t *testing.T) {
	repo := testutil.InitRepo(t)
	linked := filepath.Join(t.TempDir(), "linked-worktree")
	testutil.RunCmd(t, repo, "git", "worktree", "add", "--detach", linked)

	_, gitDir, err := gitx.DetectRepo(context.Background(), linked)
	if err != nil {
		t.Fatalf("DetectRepo error: %v", err)
	}
	commonDir, err := resolveGitCommonDir(gitDir)
	if err != nil {
		t.Fatalf("resolveGitCommonDir error: %v", err)
	}
	wantRel, err := filepath.Rel(commonDir, gitDir)
	if err != nil {
		t.Fatalf("Rel error: %v", err)
	}
	if wantRel == "." {
		t.Fatalf("expected linked worktree gitdir under common dir, got %q", gitDir)
	}

	mountRoot := filepath.Join(t.TempDir(), "mnt", "gitdir")
	mountSrc, gitDirForOps, err := planGitDirMount(gitDir, mountRoot)
	if err != nil {
		t.Fatalf("planGitDirMount error: %v", err)
	}
	if mountSrc != commonDir {
		t.Fatalf("expected mount source %q got %q", commonDir, mountSrc)
	}
	gotRel, err := filepath.Rel(mountRoot, gitDirForOps)
	if err != nil {
		t.Fatalf("Rel error: %v", err)
	}
	if gotRel != wantRel {
		t.Fatalf("expected relocated gitdir rel %q got %q", wantRel, gotRel)
	}

	copyRoot := t.TempDir()
	copyDirTree(t, mountSrc, copyRoot)
	gotTop := runGitWithDir(t, linked, filepath.Join(copyRoot, gotRel), "rev-parse", "--show-toplevel")
	if gotTop != linked {
		t.Fatalf("expected linked worktree root %q got %q", linked, gotTop)
	}
}

func TestApplyPatchDataRetriesWithoutEnglishErrorString(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.WriteFile(t, filepath.Join(repoRoot, "same.txt"), "same\n")
	patch := strings.Join([]string{
		"diff --git a/same.txt b/same.txt",
		"new file mode 100644",
		"index 0000000..2e65efe",
		"--- /dev/null",
		"+++ b/same.txt",
		"@@ -0,0 +1 @@",
		"+same",
		"",
	}, "\n")
	g := &applyRetryGitOps{
		applyErrs: []error{errors.New("Datei existiert bereits")},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := applyPatchData(context.Background(), g, model.ApplyStrict, []byte(patch), repoRoot, "", log)
	if err != nil {
		t.Fatalf("expected identical existing new file to be skipped despite localized error, got %v", err)
	}
	if g.applyCalls != 1 {
		t.Fatalf("expected a single apply attempt when filtered patch is empty, got %d", g.applyCalls)
	}
}

func TestApplyPatchDataRejectsUnsafePathBeforeApply(t *testing.T) {
	g := &applyRetryGitOps{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := applyPatchData(
		context.Background(),
		g,
		model.ApplyStrict,
		[]byte("diff --git a/../evil b/../evil\n"),
		t.TempDir(),
		"",
		log,
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if g.applyCalls != 0 {
		t.Fatalf("expected no apply attempts, got %d", g.applyCalls)
	}
}

func TestApplyPatchDataReturnsOriginalErrorWhenNothingWasSkipped(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.WriteFile(t, filepath.Join(repoRoot, "tracked.txt"), "before\n")
	patch := strings.Join([]string{
		"diff --git a/tracked.txt b/tracked.txt",
		"index df967b9..3b18e51 100644",
		"--- a/tracked.txt",
		"+++ b/tracked.txt",
		"@@ -1 +1 @@",
		"-before",
		"+after",
		"",
	}, "\n")
	wantErr := errors.New("apply failed")
	g := &applyRetryGitOps{applyErrs: []error{wantErr}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := applyPatchData(context.Background(), g, model.ApplyStrict, []byte(patch), repoRoot, "", log)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected original error %v, got %v", wantErr, err)
	}
	if g.applyCalls != 1 {
		t.Fatalf("expected one apply attempt, got %d", g.applyCalls)
	}
}

func TestApplyPatchDataReturnsRetryError(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.WriteFile(t, filepath.Join(repoRoot, "same.txt"), "same\n")
	patch := strings.Join([]string{
		"diff --git a/same.txt b/same.txt",
		"new file mode 100644",
		"index 0000000..2e65efe",
		"--- /dev/null",
		"+++ b/same.txt",
		"@@ -0,0 +1 @@",
		"+same",
		"diff --git a/other.txt b/other.txt",
		"new file mode 100644",
		"index 0000000..3e75765",
		"--- /dev/null",
		"+++ b/other.txt",
		"@@ -0,0 +1 @@",
		"+other",
		"",
	}, "\n")
	firstErr := errors.New("first apply failed")
	retryErr := errors.New("retry failed")
	g := &applyRetryGitOps{applyErrs: []error{firstErr, retryErr}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := applyPatchData(context.Background(), g, model.ApplyStrict, []byte(patch), repoRoot, "", log)
	if !errors.Is(err, retryErr) {
		t.Fatalf("expected retry error %v, got %v", retryErr, err)
	}
	if g.applyCalls != 2 {
		t.Fatalf("expected two apply attempts, got %d", g.applyCalls)
	}
}

func TestApplyPatchDataEmptyPatchIsNoop(t *testing.T) {
	g := &applyRetryGitOps{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := applyPatchData(context.Background(), g, model.ApplyStrict, nil, t.TempDir(), "", log)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if g.applyCalls != 0 {
		t.Fatalf("expected no apply attempts, got %d", g.applyCalls)
	}
}

func TestMaskIgnoredFilesReadonlySkipsMissingTargets(t *testing.T) {
	repoRoot := t.TempDir()
	existing := filepath.Join(repoRoot, "existing.txt")
	if err := os.WriteFile(existing, []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write existing: %v", err)
	}
	g := ignoredListGitOps{ignored: []string{"missing.txt", "existing.txt"}}
	mount := &recordingNSOps{}
	opts := model.Options{IgnoredMode: model.IgnoredReadonly, IgnoredMax: 10}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	targets, ignored, err := maskIgnoredFiles(context.Background(), g, repoRoot, "", "", "", opts, mount, log)
	if err != nil {
		t.Fatalf("maskIgnoredFiles error: %v", err)
	}
	if len(ignored) != 2 || ignored[0] != "missing.txt" || ignored[1] != "existing.txt" {
		t.Fatalf("expected ignored list preserved, got %v", ignored)
	}
	if len(targets) != 1 || targets[0] != existing {
		t.Fatalf("expected only existing target, got %v", targets)
	}
	if len(mount.bindCalls) != 1 || mount.bindCalls[0] != existing {
		t.Fatalf("expected bind only for existing target, got %v", mount.bindCalls)
	}
	if len(mount.remountCalls) != 1 || mount.remountCalls[0] != existing {
		t.Fatalf("expected remount only for existing target, got %v", mount.remountCalls)
	}
}

func TestMaskIgnoredFilesDisabledWhenIgnoredMaxZero(t *testing.T) {
	repoRoot := t.TempDir()
	g := ignoredListGitOps{err: errors.New("should not list ignored candidates")}
	mount := &recordingNSOps{}
	opts := model.Options{IgnoredMode: model.IgnoredReadonly, IgnoredMax: 0}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	targets, ignored, err := maskIgnoredFiles(context.Background(), g, repoRoot, "", "", "", opts, mount, log)
	if err != nil {
		t.Fatalf("maskIgnoredFiles error: %v", err)
	}
	if len(targets) != 0 || len(ignored) != 0 {
		t.Fatalf("expected ignored processing disabled, got targets=%v ignored=%v", targets, ignored)
	}
	if len(mount.bindCalls) != 0 || len(mount.remountCalls) != 0 {
		t.Fatalf("expected no mount calls, got bind=%v remount=%v", mount.bindCalls, mount.remountCalls)
	}
}

func TestMaskIgnoredFilesFailsWhenIgnoredCandidateCapExceeded(t *testing.T) {
	repoRoot := t.TempDir()
	g := ignoredListGitOps{ignored: []string{"a.tmp", "b.tmp", "c.tmp"}}
	mount := &recordingNSOps{}
	opts := model.Options{IgnoredMode: model.IgnoredReadonly, IgnoredMax: 2}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	targets, ignored, err := maskIgnoredFiles(context.Background(), g, repoRoot, "", "", "", opts, mount, log)
	if err == nil {
		t.Fatal("expected ignored-max overflow to fail")
	}
	if !strings.Contains(err.Error(), "ignored-max 2") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("expected no targets, got %v", targets)
	}
	if len(ignored) != 3 {
		t.Fatalf("expected overflow sentinel list, got %v", ignored)
	}
	if len(mount.bindCalls) != 0 || len(mount.remountCalls) != 0 {
		t.Fatalf("expected no mount calls, got bind=%v remount=%v", mount.bindCalls, mount.remountCalls)
	}
}

func TestMaskIgnoredFilesReadonlyRejectsSymlink(t *testing.T) {
	repoRoot := t.TempDir()
	target := filepath.Join(repoRoot, "target.txt")
	if err := os.WriteFile(target, []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink("target.txt", filepath.Join(repoRoot, "ignored-link.txt")); err != nil {
		t.Fatalf("symlink ignored target: %v", err)
	}
	g := ignoredListGitOps{ignored: []string{"ignored-link.txt"}}
	mount := &recordingNSOps{}
	opts := model.Options{IgnoredMode: model.IgnoredReadonly, IgnoredMax: 10}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	targets, ignored, err := maskIgnoredFiles(context.Background(), g, repoRoot, "", "", "", opts, mount, log)
	if err == nil {
		t.Fatal("expected symlink readonly rejection")
	}
	if !strings.Contains(err.Error(), "ignored readonly symlink ignored-link.txt") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("expected no targets, got %v", targets)
	}
	if len(ignored) != 1 || ignored[0] != "ignored-link.txt" {
		t.Fatalf("expected ignored list preserved, got %v", ignored)
	}
	if len(mount.bindCalls) != 0 || len(mount.remountCalls) != 0 {
		t.Fatalf("expected no mount calls, got bind=%v remount=%v", mount.bindCalls, mount.remountCalls)
	}
}

func TestMaskIgnoredFilesReadonlySkipsBindMountENOENT(t *testing.T) {
	repoRoot := t.TempDir()
	raced := filepath.Join(repoRoot, "raced.txt")
	if err := os.WriteFile(raced, []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write raced: %v", err)
	}
	existing := filepath.Join(repoRoot, "existing.txt")
	if err := os.WriteFile(existing, []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write existing: %v", err)
	}
	g := ignoredListGitOps{ignored: []string{"raced.txt", "existing.txt"}}
	mount := &recordingNSOps{
		bindErrs: map[string]error{
			raced: os.ErrNotExist,
		},
	}
	opts := model.Options{IgnoredMode: model.IgnoredReadonly, IgnoredMax: 10}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	targets, ignored, err := maskIgnoredFiles(context.Background(), g, repoRoot, "", "", "", opts, mount, log)
	if err != nil {
		t.Fatalf("maskIgnoredFiles error: %v", err)
	}
	if len(ignored) != 2 || ignored[0] != "raced.txt" || ignored[1] != "existing.txt" {
		t.Fatalf("expected ignored list preserved, got %v", ignored)
	}
	if len(targets) != 1 || targets[0] != existing {
		t.Fatalf("expected only existing target after bind ENOENT, got %v", targets)
	}
	if len(mount.bindCalls) != 2 || mount.bindCalls[0] != raced || mount.bindCalls[1] != existing {
		t.Fatalf("unexpected bind calls: %v", mount.bindCalls)
	}
	if len(mount.remountCalls) != 1 || mount.remountCalls[0] != existing {
		t.Fatalf("expected remount only for existing target, got %v", mount.remountCalls)
	}
}

func TestMaskIgnoredFilesMaskedFailsOnNonENOENTStat(t *testing.T) {
	repoRoot := t.TempDir()
	blocker := filepath.Join(repoRoot, "blocker")
	if err := os.WriteFile(blocker, []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	g := ignoredListGitOps{ignored: []string{"blocker/child.txt"}}
	mount := &recordingNSOps{}
	opts := model.Options{IgnoredMode: model.IgnoredMasked, IgnoredMax: 10}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	targets, ignored, err := maskIgnoredFiles(context.Background(), g, repoRoot, "", "", "", opts, mount, log)
	if err == nil {
		t.Fatalf("expected stat error for masked ignored path")
	}
	if !strings.Contains(err.Error(), "stat ignored masked blocker/child.txt") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("expected no mask targets on stat error, got %v", targets)
	}
	if len(ignored) != 1 || ignored[0] != "blocker/child.txt" {
		t.Fatalf("expected ignored list preserved on error, got %v", ignored)
	}
	if len(mount.bindCalls) != 0 {
		t.Fatalf("expected no bind calls on stat error, got %v", mount.bindCalls)
	}
}

func TestMaskIgnoredFilesMaskedRejectsSymlink(t *testing.T) {
	repoRoot := t.TempDir()
	targetDir := filepath.Join(repoRoot, "target-dir")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("mkdir target dir: %v", err)
	}
	if err := os.Symlink("target-dir", filepath.Join(repoRoot, "ignored-link")); err != nil {
		t.Fatalf("symlink ignored dir: %v", err)
	}
	g := ignoredListGitOps{ignored: []string{"ignored-link"}}
	mount := &recordingNSOps{}
	opts := model.Options{IgnoredMode: model.IgnoredMasked, IgnoredMax: 10}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	targets, ignored, err := maskIgnoredFiles(context.Background(), g, repoRoot, "", t.TempDir(), t.TempDir(), opts, mount, log)
	if err == nil {
		t.Fatal("expected symlink masked rejection")
	}
	if !strings.Contains(err.Error(), "ignored masked symlink ignored-link") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("expected no targets, got %v", targets)
	}
	if len(ignored) != 1 || ignored[0] != "ignored-link" {
		t.Fatalf("expected ignored list preserved, got %v", ignored)
	}
	if len(mount.maskCalls) != 0 {
		t.Fatalf("expected no mask calls, got %v", mount.maskCalls)
	}
}

func TestMaskIgnoredFilesMaskedUsesDistinctBackingPerTarget(t *testing.T) {
	repoRoot := t.TempDir()
	for _, name := range []string{"one.txt", "two.txt"} {
		if err := os.WriteFile(filepath.Join(repoRoot, name), []byte("seed\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	fileRoot := filepath.Join(t.TempDir(), "files")
	dirRoot := filepath.Join(t.TempDir(), "dirs")
	if err := os.MkdirAll(fileRoot, 0o755); err != nil {
		t.Fatalf("mkdir file root: %v", err)
	}
	if err := os.MkdirAll(dirRoot, 0o755); err != nil {
		t.Fatalf("mkdir dir root: %v", err)
	}
	g := ignoredListGitOps{ignored: []string{"one.txt", "two.txt"}}
	mount := &recordingNSOps{}
	opts := model.Options{IgnoredMode: model.IgnoredMasked, IgnoredMax: 10}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	targets, ignored, err := maskIgnoredFiles(context.Background(), g, repoRoot, "", fileRoot, dirRoot, opts, mount, log)
	if err != nil {
		t.Fatalf("maskIgnoredFiles error: %v", err)
	}
	if len(targets) != 2 || len(ignored) != 2 || len(mount.maskCalls) != 2 {
		t.Fatalf("expected two masked targets, got targets=%v ignored=%v maskCalls=%v", targets, ignored, mount.maskCalls)
	}
	if mount.maskCalls[0].emptyFile == mount.maskCalls[1].emptyFile {
		t.Fatalf("expected distinct file backings, got %q", mount.maskCalls[0].emptyFile)
	}
}

func TestCleanupStackRunOrderAndErrors(t *testing.T) {
	var c cleanupStack
	var order []int
	errA := errors.New("a")
	errB := errors.New("b")
	c.Push(func() error {
		order = append(order, 1)
		return errA
	})
	c.Push(func() error {
		order = append(order, 2)
		return nil
	})
	c.Push(func() error {
		order = append(order, 3)
		return errB
	})

	err := c.Run()
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("expected joined errors containing errA and errB, got %v", err)
	}
	if len(order) != 3 {
		t.Fatalf("expected 3 cleanup calls, got %d", len(order))
	}
	if order[0] != 3 || order[1] != 2 || order[2] != 1 {
		t.Fatalf("unexpected cleanup order: %v", order)
	}
}

func TestRunCommandExitCodes(t *testing.T) {
	t.Run("no command", func(t *testing.T) {
		if code := runCommand(t.TempDir(), ".", nil); code != 0 {
			t.Fatalf("expected 0 got %d", code)
		}
	})
	t.Run("success", func(t *testing.T) {
		if code := runCommand(t.TempDir(), ".", []string{"sh", "-c", "true"}); code != 0 {
			t.Fatalf("expected 0 got %d", code)
		}
	})
	t.Run("exit code propagated", func(t *testing.T) {
		if code := runCommand(t.TempDir(), ".", []string{"sh", "-c", "exit 7"}); code != 7 {
			t.Fatalf("expected 7 got %d", code)
		}
	})
	t.Run("persona reserved child exit codes remapped", func(t *testing.T) {
		if code := runCommand(t.TempDir(), ".", []string{"sh", "-c", "exit 12"}); code != 242 {
			t.Fatalf("expected 242 got %d", code)
		}
	})
	t.Run("signal exit normalized", func(t *testing.T) {
		if code := runCommand(t.TempDir(), ".", []string{"sh", "-c", "kill -TERM $$"}); code != 143 {
			t.Fatalf("expected 143 got %d", code)
		}
	})
	t.Run("missing command returns 127", func(t *testing.T) {
		if code := runCommand(t.TempDir(), ".", []string{"/definitely/missing/persona-test-command"}); code != 127 {
			t.Fatalf("expected 127 got %d", code)
		}
	})
	t.Run("uses cwdRel", func(t *testing.T) {
		repo := t.TempDir()
		sub := filepath.Join(repo, "sub")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir sub: %v", err)
		}
		code := runCommand(repo, "sub", []string{"sh", "-c", "pwd > cwd.txt"})
		if code != 0 {
			t.Fatalf("expected 0 got %d", code)
		}
		data, err := os.ReadFile(filepath.Join(sub, "cwd.txt"))
		if err != nil {
			t.Fatalf("read cwd.txt: %v", err)
		}
		if strings.TrimSpace(string(data)) != sub {
			t.Fatalf("expected cwd %q got %q", sub, strings.TrimSpace(string(data)))
		}
	})
	t.Run("sanitizes child git environment", func(t *testing.T) {
		repo := t.TempDir()
		t.Setenv("GIT_DIR", "/tmp/nogit-dir")
		t.Setenv("GIT_WORK_TREE", "/tmp/nogit-worktree")
		t.Setenv("GIT_INDEX_FILE", "/tmp/nogit-index")
		code := runCommand(repo, ".", []string{"sh", "-c", "env > child.env"})
		if code != 0 {
			t.Fatalf("expected 0 got %d", code)
		}
		data, err := os.ReadFile(filepath.Join(repo, "child.env"))
		if err != nil {
			t.Fatalf("read child.env: %v", err)
		}
		text := string(data)
		for _, key := range []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE="} {
			if strings.Contains(text, key) {
				t.Fatalf("expected %s to be removed from child env: %s", key, text)
			}
		}
	})
	t.Run("quiesces background descendants", func(t *testing.T) {
		repo := t.TempDir()
		code := runCommand(repo, ".", []string{"sh", "-c", `(trap '' TERM; sleep 0.3; echo late > late.txt) & exit 0`})
		if code != 0 {
			t.Fatalf("expected 0 got %d", code)
		}
		time.Sleep(500 * time.Millisecond)
		if _, err := os.Stat(filepath.Join(repo, "late.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected late.txt to stay absent, got err=%v", err)
		}
	})
}

func TestResolvePath(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.patch")
		if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
			t.Fatalf("write target: %v", err)
		}
		link := filepath.Join(dir, "state.patch")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		got := resolvePath(link)
		if got != target {
			t.Fatalf("expected %q got %q", target, got)
		}
	})
	t.Run("multi hop symlink resolves final target", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "final.patch")
		if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
			t.Fatalf("write target: %v", err)
		}
		link2 := filepath.Join(dir, "link2.patch")
		if err := os.Symlink(target, link2); err != nil {
			t.Fatalf("symlink link2: %v", err)
		}
		link1 := filepath.Join(dir, "link1.patch")
		if err := os.Symlink(link2, link1); err != nil {
			t.Fatalf("symlink link1: %v", err)
		}
		got := resolvePath(link1)
		if got != target {
			t.Fatalf("expected %q got %q", target, got)
		}
	})
	t.Run("dangling multi hop symlink resolves final alias target", func(t *testing.T) {
		dir := t.TempDir()
		link2 := filepath.Join(dir, "link2.patch")
		if err := os.Symlink(filepath.Join("nested", "final.patch"), link2); err != nil {
			t.Fatalf("symlink link2: %v", err)
		}
		link1 := filepath.Join(dir, "link1.patch")
		if err := os.Symlink("link2.patch", link1); err != nil {
			t.Fatalf("symlink link1: %v", err)
		}
		want := filepath.Join(dir, "nested", "final.patch")
		got := resolvePath(link1)
		if got != want {
			t.Fatalf("expected %q got %q", want, got)
		}
	})
	t.Run("missing path keeps input", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.patch")
		got := resolvePath(path)
		if got != path {
			t.Fatalf("expected %q got %q", path, got)
		}
	})
}

func TestExportPatchSortAndExclude(t *testing.T) {
	repo := testutil.InitRepo(t)
	testutil.WriteFile(t, filepath.Join(repo, "b.txt"), "b\n")
	testutil.WriteFile(t, filepath.Join(repo, "a.txt"), "a\n")
	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "seed\n")

	g := &gitx.Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	patch1, err := exportPatch(context.Background(), g, repo, g.GitDir, true, "state.patch", model.IgnoredTransparent, 0, nil)
	if err != nil {
		t.Fatalf("exportPatch error: %v", err)
	}
	if bytes.Contains(patch1, []byte("state.patch")) {
		t.Fatalf("expected excluded patch file")
	}
	aIdx := bytes.Index(patch1, []byte("+++ b/a.txt"))
	bIdx := bytes.Index(patch1, []byte("+++ b/b.txt"))
	if aIdx == -1 || bIdx == -1 {
		t.Fatalf("missing expected files in patch: %s", string(patch1))
	}
	if aIdx > bIdx {
		t.Fatalf("expected deterministic sorted order a.txt before b.txt")
	}
	if bytes.Contains(patch1, []byte("+++ b/.git/")) {
		t.Fatalf("unexpected .git path in patch")
	}

	patch2, err := exportPatch(context.Background(), g, repo, g.GitDir, true, "state.patch", model.IgnoredTransparent, 0, nil)
	if err != nil {
		t.Fatalf("exportPatch second call error: %v", err)
	}
	if !bytes.Equal(patch1, patch2) {
		t.Fatalf("expected deterministic output across runs")
	}
}

func TestExportPatchIncludesPatchFileWhenNotExcluded(t *testing.T) {
	repo := testutil.InitRepo(t)
	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "seed\n")

	g := &gitx.Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	patch, err := exportPatch(context.Background(), g, repo, g.GitDir, false, "", model.IgnoredTransparent, 0, nil)
	if err != nil {
		t.Fatalf("exportPatch error: %v", err)
	}
	if !bytes.Contains(patch, []byte("state.patch")) {
		t.Fatalf("expected state.patch to be included when not excluded")
	}
}

func TestExportPatchSkipsVanishedUntrackedFiles(t *testing.T) {
	repo := t.TempDir()
	testutil.WriteFile(t, filepath.Join(repo, "keep.txt"), "keep\n")
	g := exportGitOps{
		untracked: []string{"gone.txt", "keep.txt"},
		diffByPath: map[string][]byte{
			"keep.txt": []byte("diff --git a/keep.txt b/keep.txt\n+++ b/keep.txt\n"),
		},
		errByPath: map[string]error{
			"gone.txt": os.ErrNotExist,
		},
	}

	patch, err := exportPatch(context.Background(), g, repo, "", false, "", model.IgnoredTransparent, 0, nil)
	if err != nil {
		t.Fatalf("exportPatch error: %v", err)
	}
	if !bytes.Contains(patch, []byte("keep.txt")) {
		t.Fatalf("expected surviving untracked file in patch: %s", string(patch))
	}
	if bytes.Contains(patch, []byte("gone.txt")) {
		t.Fatalf("expected vanished untracked file to be skipped: %s", string(patch))
	}
}

func TestExportPatchFailsOnNewIgnoredCandidates(t *testing.T) {
	g := exportGitOps{
		ignored: []string{"late-ignored.txt"},
	}

	_, err := exportPatch(context.Background(), g, t.TempDir(), "", false, "", model.IgnoredReadonly, 1, nil)
	if err == nil {
		t.Fatalf("expected error when new ignored candidates appear after child run")
	}
	if !strings.Contains(err.Error(), "late-ignored.txt") {
		t.Fatalf("expected ignored path in error, got %v", err)
	}
}

func TestExportPatchFailsWhenIgnoredCandidateCapExceeded(t *testing.T) {
	g := exportGitOps{
		ignored: []string{"a.tmp", "b.tmp", "c.tmp"},
	}

	_, err := exportPatch(context.Background(), g, t.TempDir(), "", false, "", model.IgnoredReadonly, 2, []string{"a.tmp", "b.tmp"})
	if err == nil {
		t.Fatal("expected ignored-max overflow to fail")
	}
	if !strings.Contains(err.Error(), "ignored-max 2") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExportPatchIgnoredDriftDetectsNewCandidatesWithinIgnoredMax(t *testing.T) {
	g := exportGitOps{
		ignored: []string{"a.tmp", "b.tmp"},
	}

	_, err := exportPatch(context.Background(), g, t.TempDir(), "", false, "", model.IgnoredReadonly, 2, []string{"a.tmp"})
	if err == nil {
		t.Fatalf("expected new ignored candidate within ignored-max to fail")
	}
	if !strings.Contains(err.Error(), "b.tmp") {
		t.Fatalf("expected new ignored path in error, got %v", err)
	}
}

func TestExportPatchFailsOnIgnoredToUnignoredTransition(t *testing.T) {
	g := exportGitOps{
		ignored: []string{"keep.tmp"},
	}

	_, err := exportPatch(context.Background(), g, t.TempDir(), "", false, "", model.IgnoredReadonly, 2, []string{"keep.tmp", "gone.tmp"})
	if err == nil {
		t.Fatal("expected ignored-to-unignored transition to fail")
	}
	if !strings.Contains(err.Error(), "gone.tmp") || !strings.Contains(err.Error(), "no longer ignored") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExportPatchIgnoredMaxZeroSkipsIgnoredDriftCheck(t *testing.T) {
	g := exportGitOps{
		ignoredErr: errors.New("should not list ignored candidates"),
	}

	patch, err := exportPatch(context.Background(), g, t.TempDir(), "", false, "", model.IgnoredReadonly, 0, nil)
	if err != nil {
		t.Fatalf("exportPatch error: %v", err)
	}
	if len(patch) != 0 {
		t.Fatalf("expected empty patch, got %q", string(patch))
	}
}

func TestExportPatchFailsWhenAccumulatedDiffExceedsSizeLimit(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "late.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write late.txt: %v", err)
	}
	g := exportGitOps{
		tracked:   bytes.Repeat([]byte("t"), patchio.MaxPatchBytes-8),
		untracked: []string{"late.txt"},
		diffByPath: map[string][]byte{
			"late.txt": bytes.Repeat([]byte("u"), 16),
		},
	}

	_, err := exportPatch(context.Background(), g, repo, "", false, "", model.IgnoredTransparent, 0, nil)
	if err == nil {
		t.Fatal("expected accumulated patch size overflow to fail")
	}
	if !strings.Contains(err.Error(), "patch exceeds size limit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExportPatchExcludesTrackedPatchState(t *testing.T) {
	repo := testutil.InitRepo(t)
	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "seed\n")
	testutil.WriteFile(t, filepath.Join(repo, "state.patch.lock"), "lock\n")
	testutil.RunCmd(t, repo, "git", "add", "state.patch")
	testutil.RunCmd(t, repo, "git", "add", "state.patch.lock")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "track patch")
	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "updated\n")
	testutil.WriteFile(t, filepath.Join(repo, "state.patch.lock"), "updated lock\n")
	testutil.WriteFile(t, filepath.Join(repo, "other.txt"), "other\n")

	g := &gitx.Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	patch, err := exportPatch(context.Background(), g, repo, g.GitDir, true, "state.patch", model.IgnoredTransparent, 0, nil)
	if err != nil {
		t.Fatalf("exportPatch error: %v", err)
	}
	if bytes.Contains(patch, []byte("state.patch")) {
		t.Fatalf("expected tracked patch file to be excluded from export: %s", string(patch))
	}
	if bytes.Contains(patch, []byte("state.patch.lock")) {
		t.Fatalf("expected tracked patch lock file to be excluded from export: %s", string(patch))
	}
	if !bytes.Contains(patch, []byte("other.txt")) {
		t.Fatalf("expected other changes to remain in export: %s", string(patch))
	}
}

func TestExportPatchExcludesUntrackedPatchLockFile(t *testing.T) {
	repo := testutil.InitRepo(t)
	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "")
	testutil.WriteFile(t, filepath.Join(repo, "state.patch.lock"), "lock\n")
	testutil.WriteFile(t, filepath.Join(repo, "other.txt"), "other\n")

	g := &gitx.Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	patch, err := exportPatch(context.Background(), g, repo, g.GitDir, true, "state.patch", model.IgnoredTransparent, 0, nil)
	if err != nil {
		t.Fatalf("exportPatch error: %v", err)
	}
	if bytes.Contains(patch, []byte("state.patch.lock")) {
		t.Fatalf("expected untracked patch lock file to be excluded from export: %s", string(patch))
	}
	if !bytes.Contains(patch, []byte("other.txt")) {
		t.Fatalf("expected other changes to remain in export: %s", string(patch))
	}
}
