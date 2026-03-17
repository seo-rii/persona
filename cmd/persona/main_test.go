package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"persona/internal/gitx"
	"persona/internal/model"
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

type buildOptionsInput struct {
	patchPath      string
	patchDir       string
	printPatchPath bool
	baseMode       string
	baseRef        string
	allowDirty     bool
	ignoredMode    string
	ignoredMax     int
	ignoredScope   string
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
	diffByPath map[string][]byte
	errByPath  map[string]error
}

func (g ignoredListGitOps) RepoRootPath() string { return "" }

func (g ignoredListGitOps) GitDirPath() string { return "" }

func (g ignoredListGitOps) IsCleanExceptUntracked(context.Context, []string) (bool, error) {
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

func (g ignoredListGitOps) ListIgnoredCandidates(context.Context, string, string, int) ([]string, error) {
	return g.ignored, g.err
}

func (g exportGitOps) RepoRootPath() string { return "" }

func (g exportGitOps) GitDirPath() string { return "" }

func (g exportGitOps) IsCleanExceptUntracked(context.Context, []string) (bool, error) {
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
	if maxN > 0 && len(g.ignored) > maxN {
		return append([]string(nil), g.ignored[:maxN]...), nil
	}
	return append([]string(nil), g.ignored...), nil
}

type recordingNSOps struct {
	bindCalls    []string
	remountCalls []string
	bindErrs     map[string]error
	remountErrs  map[string]error
}

type worktreeGitOps struct {
	addCalls    []string
	removeCalls []string
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

func (g *applyRetryGitOps) RepoRootPath() string { return "" }

func (g *applyRetryGitOps) GitDirPath() string { return "" }

func (g *applyRetryGitOps) IsCleanExceptUntracked(context.Context, []string) (bool, error) {
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

func (m *recordingNSOps) MaskPath(string, model.MaskKind, string, string) error { return nil }

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
		ignoredScope:   "exact",
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
		in.ignoredMode, in.ignoredMax, in.ignoredScope,
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
			name: "invalid ignored scope",
			want: "ignored-scope",
			mutate: func(in *buildOptionsInput) {
				in.ignoredScope = "all"
			},
		},
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
	patch1, err := exportPatch(context.Background(), g, repo, g.GitDir, true, "state.patch", model.IgnoredTransparent, nil)
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

	patch2, err := exportPatch(context.Background(), g, repo, g.GitDir, true, "state.patch", model.IgnoredTransparent, nil)
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
	patch, err := exportPatch(context.Background(), g, repo, g.GitDir, false, "", model.IgnoredTransparent, nil)
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

	patch, err := exportPatch(context.Background(), g, repo, "", false, "", model.IgnoredTransparent, nil)
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

	_, err := exportPatch(context.Background(), g, t.TempDir(), "", false, "", model.IgnoredReadonly, nil)
	if err == nil {
		t.Fatalf("expected error when new ignored candidates appear after child run")
	}
	if !strings.Contains(err.Error(), "late-ignored.txt") {
		t.Fatalf("expected ignored path in error, got %v", err)
	}
}

func TestExportPatchIgnoredDriftUsesBaselineCap(t *testing.T) {
	g := exportGitOps{
		ignored: []string{"a.tmp", "b.tmp", "c.tmp"},
	}

	_, err := exportPatch(context.Background(), g, t.TempDir(), "", false, "", model.IgnoredReadonly, []string{"a.tmp", "b.tmp"})
	if err != nil {
		t.Fatalf("expected capped ignored baseline not to fail, got %v", err)
	}
}

func TestExportPatchExcludesTrackedPatchFile(t *testing.T) {
	repo := testutil.InitRepo(t)
	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "seed\n")
	testutil.RunCmd(t, repo, "git", "add", "state.patch")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "track patch")
	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "updated\n")
	testutil.WriteFile(t, filepath.Join(repo, "other.txt"), "other\n")

	g := &gitx.Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	patch, err := exportPatch(context.Background(), g, repo, g.GitDir, true, "state.patch", model.IgnoredTransparent, nil)
	if err != nil {
		t.Fatalf("exportPatch error: %v", err)
	}
	if bytes.Contains(patch, []byte("state.patch")) {
		t.Fatalf("expected tracked patch file to be excluded from export: %s", string(patch))
	}
	if !bytes.Contains(patch, []byte("other.txt")) {
		t.Fatalf("expected other changes to remain in export: %s", string(patch))
	}
}
