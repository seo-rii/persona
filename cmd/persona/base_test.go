package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"persona/internal/gitx"
	"persona/internal/model"
	"persona/internal/session"
	"persona/internal/testutil"
)

func TestPrepareBasePinsWorktreeExportAndRecovery(t *testing.T) {
	repo := testutil.InitRepo(t)
	testutil.RunCmd(t, repo, "git", "tag", "old-base")
	testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "new original HEAD\n")
	testutil.WriteFile(t, filepath.Join(repo, "head-only.txt"), "new HEAD only\n")
	testutil.RunCmd(t, repo, "git", "add", ".")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "new HEAD")
	ctx := context.Background()
	g := &gitx.Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	opts := model.Options{BaseMode: model.BaseWorktree, BaseRef: "old-base"}
	sess := &session.Session{BaseWT: filepath.Join(t.TempDir(), "base")}
	cleanup := &cleanupStack{}
	defer cleanup.Run()
	var retErr error
	basePath, err := prepareBase(ctx, g, opts, sess, false, "", cleanup, &retErr)
	if err != nil {
		t.Fatal(err)
	}
	if g.BaseOID == "" || g.BaseOID == opts.BaseRef {
		t.Fatalf("base was not pinned to an object ID: %q", g.BaseOID)
	}
	patch, err := exportPatchBytes(ctx, g, basePath, g.GitDir, false, "", 0, nil)
	if err != nil || len(patch) != 0 {
		t.Fatalf("unmodified historical base exported changes: %v\n%s", err, patch)
	}
	// Persisting the OID must survive both the named ref and original HEAD
	// moving between daemon runs.
	testutil.RunCmd(t, repo, "git", "tag", "-f", "old-base", "HEAD")
	remembered := g.BaseOID
	recovered := &gitx.Git{RepoRoot: repo, GitDir: g.GitDir, BaseOID: remembered}
	recoverySession := &session.Session{BaseWT: filepath.Join(t.TempDir(), "recovered")}
	path, err := prepareBase(ctx, recovered, opts, recoverySession, false, "", cleanup, &retErr)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(path, "tracked.txt"))
	if err != nil || string(data) != "base\n" {
		t.Fatalf("recovery followed a moved ref: %q %v", data, err)
	}
	if recovered.BaseOID != remembered {
		t.Fatalf("recovery changed base %s -> %s", remembered, recovered.BaseOID)
	}
}

func TestMaskIgnoredReadonlyRemountFailureRetainsCleanupTarget(t *testing.T) {
	for _, failure := range []error{os.ErrNotExist, errors.New("remount failed")} {
		t.Run(failure.Error(), func(t *testing.T) {
			repo := t.TempDir()
			path := filepath.Join(repo, "ignored.txt")
			testutil.WriteFile(t, path, "sentinel\n")
			mount := &recordingNSOps{remountErrs: map[string]error{path: failure}}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			targets, _, err := maskIgnoredFiles(context.Background(), ignoredListGitOps{ignored: []string{"ignored.txt"}}, repo, "", "", "", model.Options{IgnoredMode: model.IgnoredReadonly, IgnoredMax: 10}, mount, log)
			if err == nil || !strings.Contains(err.Error(), "remount ignored readonly") {
				t.Fatalf("partial readonly mount reported success: %v", err)
			}
			if len(targets) != 1 || targets[0] != path {
				t.Fatalf("successful bind was lost before cleanup: %v", targets)
			}
		})
	}
}

func TestCleanupPreservesTreesWhenMountsRemain(t *testing.T) {
	t.Run("external gitdir backing", func(t *testing.T) {
		root := t.TempDir()
		sentinel := filepath.Join(root, "source-git-sentinel")
		testutil.WriteFile(t, sentinel, "preserve\n")
		// /proc is an existing mount, so this checks the live mount guard
		// without requiring privilege to mount a temporary fixture.
		err := cleanupUnmounted("/proc", func() error { return os.RemoveAll(root) })
		if err == nil || !strings.Contains(err.Error(), "mounts remain") {
			t.Fatalf("mounted tree was not protected: %v", err)
		}
		if data, err := os.ReadFile(sentinel); err != nil || string(data) != "preserve\n" {
			t.Fatalf("cleanup crossed a remaining mount: %q %v", data, err)
		}
	})
	t.Run("worktree cleanup", func(t *testing.T) {
		baseWT := t.TempDir()
		sentinel := filepath.Join(baseWT, "sentinel")
		testutil.WriteFile(t, sentinel, "preserve\n")
		g := &worktreeGitOps{}
		sess := &session.Session{Root: "/proc", BaseWT: baseWT}
		cleanup := &cleanupStack{}
		var retErr error
		_, err := prepareBase(context.Background(), g, model.Options{BaseMode: model.BaseWorktree, BaseRef: "HEAD", KeepSession: model.KeepNever}, sess, false, "", cleanup, &retErr)
		if err != nil {
			t.Fatal(err)
		}
		if err := cleanup.Run(); err == nil || !strings.Contains(err.Error(), "mounts remain") {
			t.Fatalf("worktree cleanup ignored a remaining mount: %v", err)
		}
		if len(g.removeCalls) != 0 {
			t.Fatalf("Git removal ran on a mounted session: %v", g.removeCalls)
		}
		if data, err := os.ReadFile(sentinel); err != nil || string(data) != "preserve\n" {
			t.Fatalf("worktree sentinel was removed: %q %v", data, err)
		}
	})
	t.Run("unmounted tree", func(t *testing.T) {
		root := t.TempDir()
		testutil.WriteFile(t, filepath.Join(root, "sentinel"), "temporary\n")
		if err := cleanupUnmounted(root, func() error { return os.RemoveAll(root) }); err != nil {
			t.Fatalf("unmounted tree could not be cleaned: %v", err)
		}
		if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unmounted tree remains: %v", err)
		}
	})
}
