package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

func TestNewRootCmdDoesNotExposeIgnoredScopeFlag(t *testing.T) {
	cmd := newRootCmd()
	flag := cmd.Flags().Lookup("ignored-scope")
	if flag != nil {
		t.Fatal("expected ignored-scope flag to be removed")
	}
}

func TestPatchStateRelPaths(t *testing.T) {
	got := patchStateRelPaths("state.patch")
	if len(got) != 2 || got[0] != "state.patch" || got[1] != "state.patch.lock" {
		t.Fatalf("unexpected patch state paths: %v", got)
	}
	if got := patchStateRelPaths(""); got != nil {
		t.Fatalf("expected nil for empty patch path, got %v", got)
	}
}

func TestCheckIgnoredCandidateLimit(t *testing.T) {
	if err := checkIgnoredCandidateLimit([]string{"one", "two"}, 2); err != nil {
		t.Fatalf("unexpected limit error: %v", err)
	}
	err := checkIgnoredCandidateLimit([]string{"one", "two", "three"}, 2)
	if err == nil || !strings.Contains(err.Error(), "ignored-max 2") {
		t.Fatalf("expected ignored-max overflow, got %v", err)
	}
}

func TestListIgnoredCandidatesCheckedRejectsCapOverflow(t *testing.T) {
	g := ignoredListGitOps{ignored: []string{"one", "two"}}

	ignored, err := listIgnoredCandidatesChecked(context.Background(), g, "/repo", "", 1)
	if err == nil {
		t.Fatal("expected ignored-max overflow error")
	}
	if ignored != nil {
		t.Fatalf("expected nil ignored list on overflow, got %v", ignored)
	}
	if !strings.Contains(err.Error(), "ignored candidate count exceeds ignored-max 1") {
		t.Fatalf("unexpected error: %v", err)
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
	tracked         []byte
	untracked       []string
	untrackedErr    error
	ignored         []string
	ignoredErr      error
	diffByPath      map[string][]byte
	errByPath       map[string]error
	diffHeadCalls   int
	diffHeadToCalls int
	diffNewCalls    int
	diffNewToCalls  int
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

func (g ignoredListGitOps) DiffHeadBinaryTo(context.Context, string, string, []string, io.Writer) error {
	panic("unexpected call")
}

func (g ignoredListGitOps) DiffNewFileNoIndexTo(context.Context, string, string, string, io.Writer) error {
	panic("unexpected call")
}

func (g ignoredListGitOps) ApplyPatchReader(context.Context, model.ApplyMode, string, string, io.Reader) error {
	panic("unexpected call")
}

func (g ignoredListGitOps) ApplyPatchFromReader(context.Context, model.ApplyMode, string, string, io.Reader) error {
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

func (g *exportGitOps) RepoRootPath() string { return "" }

func (g *exportGitOps) GitDirPath() string { return "" }

func (g *exportGitOps) IsCleanExceptUntracked(context.Context, []string) (bool, error) {
	panic("unexpected call")
}

func (g *exportGitOps) IsCleanExceptPaths(context.Context, []string) (bool, error) {
	panic("unexpected call")
}

func (g *exportGitOps) WorktreeAddDetach(context.Context, string, string) error {
	panic("unexpected call")
}

func (g *exportGitOps) WorktreeRemoveForce(context.Context, string) error {
	panic("unexpected call")
}

func (g *exportGitOps) ApplyPatch(context.Context, model.ApplyMode, string, string, []byte) error {
	panic("unexpected call")
}

func (g *exportGitOps) DiffHeadBinary(context.Context, string, string, []string) ([]byte, error) {
	g.diffHeadCalls++
	return g.tracked, nil
}

func (g *exportGitOps) DiffHeadBinaryTo(_ context.Context, _ string, _ string, _ []string, w io.Writer) error {
	g.diffHeadToCalls++
	_, err := w.Write(g.tracked)
	return err
}

func (g *exportGitOps) ListUntracked(context.Context, string, string) ([]string, error) {
	if g.untrackedErr != nil {
		return nil, g.untrackedErr
	}
	return g.untracked, nil
}

func (g *exportGitOps) DiffNewFileNoIndex(_ context.Context, _ string, _ string, relPath string) ([]byte, error) {
	g.diffNewCalls++
	if err, ok := g.errByPath[relPath]; ok {
		return nil, err
	}
	return g.diffByPath[relPath], nil
}

func (g *exportGitOps) DiffNewFileNoIndexTo(_ context.Context, _ string, _ string, relPath string, w io.Writer) error {
	g.diffNewToCalls++
	if err, ok := g.errByPath[relPath]; ok {
		return err
	}
	_, err := w.Write(g.diffByPath[relPath])
	return err
}

func (g *exportGitOps) ApplyPatchReader(context.Context, model.ApplyMode, string, string, io.Reader) error {
	panic("unexpected call")
}

func (g *exportGitOps) ApplyPatchFromReader(context.Context, model.ApplyMode, string, string, io.Reader) error {
	panic("unexpected call")
}

func (g *exportGitOps) ListIgnoredCandidates(_ context.Context, _ string, _ string, maxN int) ([]string, error) {
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

type umountRecordingNSOps struct {
	paths []string
	errs  map[string]error
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
	applyCalls       int
	applyErrs        []error
	applyReaderCalls int
	applyReaderErrs  []error
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

func (g worktreeGitOps) DiffHeadBinaryTo(context.Context, string, string, []string, io.Writer) error {
	panic("unexpected call")
}

func (g worktreeGitOps) DiffNewFileNoIndexTo(context.Context, string, string, string, io.Writer) error {
	panic("unexpected call")
}

func (g worktreeGitOps) ApplyPatchReader(context.Context, model.ApplyMode, string, string, io.Reader) error {
	panic("unexpected call")
}

func (g worktreeGitOps) ApplyPatchFromReader(context.Context, model.ApplyMode, string, string, io.Reader) error {
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

func (g repoCleanGitOps) DiffHeadBinaryTo(context.Context, string, string, []string, io.Writer) error {
	panic("unexpected call")
}

func (g repoCleanGitOps) DiffNewFileNoIndexTo(context.Context, string, string, string, io.Writer) error {
	panic("unexpected call")
}

func (g repoCleanGitOps) ApplyPatchReader(context.Context, model.ApplyMode, string, string, io.Reader) error {
	panic("unexpected call")
}

func (g repoCleanGitOps) ApplyPatchFromReader(context.Context, model.ApplyMode, string, string, io.Reader) error {
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

func (g *applyRetryGitOps) ApplyPatchReader(ctx context.Context, mode model.ApplyMode, workTree, gitDir string, patchData io.Reader) error {
	g.applyReaderCalls++
	_, _ = io.Copy(io.Discard, patchData)
	if len(g.applyReaderErrs) >= g.applyReaderCalls {
		return g.applyReaderErrs[g.applyReaderCalls-1]
	}
	return nil
}

func (g *applyRetryGitOps) ApplyPatchFromReader(ctx context.Context, mode model.ApplyMode, workTree, gitDir string, patchData io.Reader) error {
	return g.ApplyPatchReader(ctx, mode, workTree, gitDir, patchData)
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

func (g *applyRetryGitOps) DiffHeadBinaryTo(context.Context, string, string, []string, io.Writer) error {
	panic("unexpected call")
}

func (g *applyRetryGitOps) DiffNewFileNoIndexTo(context.Context, string, string, string, io.Writer) error {
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

func (m *umountRecordingNSOps) UnshareMountNS() error { return nil }

func (m *umountRecordingNSOps) MakeMountsPrivate() error { return nil }

func (m *umountRecordingNSOps) BindMount(string, string) error { return nil }

func (m *umountRecordingNSOps) RemountRO(string) error { return nil }

func (m *umountRecordingNSOps) Umount(path string) error {
	m.paths = append(m.paths, path)
	if err, ok := m.errs[path]; ok {
		return err
	}
	return nil
}

func (m *umountRecordingNSOps) MountOverlay(string, model.OverlayOpts) error { return nil }

func (m *umountRecordingNSOps) MaskPath(string, model.MaskKind, string, string) error { return nil }

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

func TestMaskIgnoredFilesTransparentReturnsIgnoredSnapshotWithoutMounting(t *testing.T) {
	repoRoot := t.TempDir()
	g := ignoredListGitOps{ignored: []string{"cache.tmp", "logs/out.txt"}}
	mount := &recordingNSOps{}
	opts := model.Options{IgnoredMode: model.IgnoredTransparent, IgnoredMax: 10}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	targets, ignored, err := maskIgnoredFiles(context.Background(), g, repoRoot, "", "", "", opts, mount, log)
	if err != nil {
		t.Fatalf("maskIgnoredFiles error: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("expected transparent mode to avoid mount targets, got %v", targets)
	}
	if len(ignored) != 2 || ignored[0] != "cache.tmp" || ignored[1] != "logs/out.txt" {
		t.Fatalf("expected ignored snapshot, got %v", ignored)
	}
	if len(mount.bindCalls) != 0 || len(mount.maskCalls) != 0 || len(mount.remountCalls) != 0 {
		t.Fatalf("expected no mount calls, got bind=%v mask=%v remount=%v", mount.bindCalls, mount.maskCalls, mount.remountCalls)
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
	if ignored != nil {
		t.Fatalf("expected ignored list to be discarded on overflow, got %v", ignored)
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

func TestMaskPathWithBackingPreparesBackingAndMasks(t *testing.T) {
	target := filepath.Join(t.TempDir(), "masked.txt")
	fileRoot := filepath.Join(t.TempDir(), "files")
	dirRoot := filepath.Join(t.TempDir(), "dirs")
	if err := os.MkdirAll(fileRoot, 0o755); err != nil {
		t.Fatalf("mkdir file root: %v", err)
	}
	if err := os.MkdirAll(dirRoot, 0o755); err != nil {
		t.Fatalf("mkdir dir root: %v", err)
	}
	mount := &recordingNSOps{}

	if err := maskPathWithBacking(mount, target, model.MaskFile, fileRoot, dirRoot); err != nil {
		t.Fatalf("maskPathWithBacking error: %v", err)
	}
	if len(mount.maskCalls) != 1 {
		t.Fatalf("expected one mask call, got %v", mount.maskCalls)
	}
	call := mount.maskCalls[0]
	if call.target != target || call.kind != model.MaskFile {
		t.Fatalf("unexpected mask call: %+v", call)
	}
	if _, err := os.Stat(call.emptyFile); err != nil {
		t.Fatalf("expected prepared empty file, got %v", err)
	}
	info, err := os.Stat(call.emptyDir)
	if err != nil {
		t.Fatalf("expected prepared empty dir, got %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected directory backing, got %v", info.Mode())
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

func TestPushKeepSessionCleanupHonorsPolicy(t *testing.T) {
	var cleanup cleanupStack
	calls := 0
	retErr := error(nil)
	opts := model.Options{KeepSession: model.KeepOnFail}

	pushKeepSessionCleanup(&cleanup, &retErr, opts, func() error {
		calls++
		return nil
	})
	if err := cleanup.Run(); err != nil {
		t.Fatalf("cleanup run: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected cleanup to run on success, got %d calls", calls)
	}

	cleanup = cleanupStack{}
	calls = 0
	retErr = errors.New("fail")
	pushKeepSessionCleanup(&cleanup, &retErr, opts, func() error {
		calls++
		return nil
	})
	if err := cleanup.Run(); err != nil {
		t.Fatalf("cleanup run: %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected cleanup to be skipped when session is kept, got %d calls", calls)
	}
}

func TestPushUmountCleanupRunsViaCleanupStack(t *testing.T) {
	var cleanup cleanupStack
	mount := &umountRecordingNSOps{}

	pushUmountCleanup(&cleanup, mount, "/tmp/masked")
	if err := cleanup.Run(); err != nil {
		t.Fatalf("cleanup run: %v", err)
	}
	if len(mount.paths) != 1 || mount.paths[0] != "/tmp/masked" {
		t.Fatalf("unexpected umount paths: %v", mount.paths)
	}

	pushUmountCleanup(&cleanup, mount, "")
	if len(cleanup.fns) != 1 {
		t.Fatalf("expected empty path to be ignored, got %d cleanup funcs", len(cleanup.fns))
	}
}

func TestCloseAndRemoveTempFileRemovesPath(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "persona-temp-*.patch")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	name := file.Name()
	if _, err := file.WriteString("patch"); err != nil {
		t.Fatalf("write temp: %v", err)
	}

	closeAndRemoveTempFile(file)

	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected temp file to be removed, got %v", err)
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
	t.Run("forwards sigterm and reaps grandchild", func(t *testing.T) {
		repo := t.TempDir()
		pidPath := filepath.Join(repo, "grandchild.pid")
		signalResult := make(chan error, 1)
		go func() {
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				data, err := os.ReadFile(pidPath)
				if err == nil && strings.TrimSpace(string(data)) != "" {
					signalResult <- syscall.Kill(os.Getpid(), syscall.SIGTERM)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			signalResult <- fmt.Errorf("pid file was not written")
		}()

		cmd := fmt.Sprintf(`(trap '' TERM; sleep 30) & echo $! > %q; wait`, pidPath)
		if code := runCommand(repo, ".", []string{"sh", "-c", cmd}); code != 143 {
			t.Fatalf("expected 143 got %d", code)
		}
		if err := <-signalResult; err != nil {
			t.Fatalf("send sigterm: %v", err)
		}

		data, err := os.ReadFile(pidPath)
		if err != nil {
			t.Fatalf("read pid file: %v", err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			t.Fatalf("parse pid: %v", err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			err := syscall.Kill(pid, 0)
			if errors.Is(err, syscall.ESRCH) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("expected grandchild pid %d to be gone, last err=%v", pid, err)
			}
			time.Sleep(10 * time.Millisecond)
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
