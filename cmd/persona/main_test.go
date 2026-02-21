package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"persona/internal/gitx"
	"persona/internal/model"
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
	patch1, err := exportPatch(context.Background(), g, repo, g.GitDir, true, "state.patch")
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

	patch2, err := exportPatch(context.Background(), g, repo, g.GitDir, true, "state.patch")
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
	patch, err := exportPatch(context.Background(), g, repo, g.GitDir, false, "")
	if err != nil {
		t.Fatalf("exportPatch error: %v", err)
	}
	if !bytes.Contains(patch, []byte("state.patch")) {
		t.Fatalf("expected state.patch to be included when not excluded")
	}
}


