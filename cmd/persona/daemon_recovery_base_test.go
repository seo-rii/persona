package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"persona/internal/gitx"
	"persona/internal/testutil"
)

func recoveryBaseFixture(t *testing.T) (string, string, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := testutil.InitRepo(t)
	testutil.WriteFile(t, filepath.Join(repo, ".gitignore"), "*.ignored\n")
	testutil.WriteFile(t, filepath.Join(repo, ".gitattributes"), "*.txt diff=trap\n*.normalized text\n")
	testutil.WriteFile(t, filepath.Join(repo, "same.normalized"), "unchanged normalized file\n")
	if err := os.Symlink("tracked.txt", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	testutil.RunCmd(t, repo, "git", "add", ".")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "recovery base")
	g := &gitx.Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	oid, err := g.ResolveBase(context.Background(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(t.TempDir(), "retained")
	if err := g.WorktreeAddDetach(context.Background(), base, oid); err != nil {
		t.Fatal(err)
	}
	return repo, base, oid
}

func TestValidateRecoveryBaseAllowsUntouchedPinnedWorktree(t *testing.T) {
	repo, base, oid := recoveryBaseFixture(t)
	// Display settings and a moving source checkout must not invalidate an
	// intact retained worktree. The source's index is not the retained index.
	for _, setting := range [][2]string{{"diff.trap.textconv", "printf same"}, {"diff.noprefix", "true"}, {"core.filemode", "false"}, {"core.autocrlf", "true"}, {"status.showUntrackedFiles", "no"}} {
		testutil.RunCmd(t, repo, "git", "config", setting[0], setting[1])
	}
	testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "source moved\n")
	testutil.RunCmd(t, repo, "git", "add", "tracked.txt")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "source advanced")
	if err := validateRecoveryBase(context.Background(), base, oid); err != nil {
		t.Fatalf("untouched pinned worktree was rejected: %v", err)
	}
}

func TestValidateRecoveryBaseRejectsChangedLower(t *testing.T) {
	for _, kind := range []string{"contents", "assume-unchanged", "skip-worktree", "executable", "symlink", "deleted", "untracked", "ignored", "hidden-ignored", "head", "index-only", "missing-git-marker", "normalized-raw-bytes"} {
		t.Run(kind, func(t *testing.T) {
			repo, base, oid := recoveryBaseFixture(t)
			path := filepath.Join(base, "tracked.txt")
			switch kind {
			case "contents", "assume-unchanged", "skip-worktree":
				if kind != "contents" {
					testutil.RunCmd(t, base, "git", "update-index", "--"+kind, "tracked.txt")
				}
				testutil.RunCmd(t, repo, "git", "config", "diff.trap.textconv", "printf same")
				testutil.WriteFile(t, path, "altered lower\n")
			case "executable":
				testutil.RunCmd(t, repo, "git", "config", "core.filemode", "false")
				if err := os.Chmod(path, 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(filepath.Join(base, "link")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("same.normalized", filepath.Join(base, "link")); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "untracked":
				testutil.WriteFile(t, filepath.Join(base, "extra.txt"), "extra\n")
			case "ignored", "hidden-ignored":
				if kind == "hidden-ignored" {
					testutil.RunCmd(t, repo, "git", "config", "status.showUntrackedFiles", "no")
				}
				testutil.WriteFile(t, filepath.Join(base, "extra.ignored"), "extra\n")
			case "head":
				testutil.RunCmd(t, base, "git", "commit", "--allow-empty", "-m", "changed lower HEAD")
			case "index-only":
				testutil.WriteFile(t, path, "staged index drift\n")
				testutil.RunCmd(t, base, "git", "add", "tracked.txt")
				testutil.WriteFile(t, path, "base\n")
			case "missing-git-marker":
				if err := os.Remove(filepath.Join(base, ".git")); err != nil {
					t.Fatal(err)
				}
			case "normalized-raw-bytes":
				testutil.WriteFile(t, filepath.Join(base, "same.normalized"), "unchanged normalized file\r\n")
			}
			if err := validateRecoveryBase(context.Background(), base, oid); err == nil {
				t.Fatal("changed lower was accepted for upper recovery")
			}
			if _, err := os.Stat(base); err != nil {
				t.Fatalf("rejected lower was removed: %v", err)
			}
		})
	}
}
