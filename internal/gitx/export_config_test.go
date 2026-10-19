package gitx

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"persona/internal/model"
	"persona/internal/testutil"
)

func TestWorktreeBasePreservesSymlinksBeforeExport(t *testing.T) {
	isolateGitConfig(t)
	ctx := context.Background()
	repo := testutil.InitRepo(t)
	if err := os.Symlink("tracked.txt", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	testutil.RunCmd(t, repo, "git", "add", ".")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "base symlink")
	testutil.RunCmd(t, repo, "git", "config", "core.symlinks", "false")
	g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	if _, err := g.ResolveBase(ctx, "HEAD"); err != nil {
		t.Fatal(err)
	}
	view := filepath.Join(t.TempDir(), "view")
	if err := g.WorktreeAddDetach(ctx, view, g.BaseOID); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(view, "link")); err != nil || target != "tracked.txt" {
		t.Fatalf("worktree changed committed symlink: target=%q err=%v", target, err)
	}
	if patch := exportAll(t, g, view); len(patch) != 0 {
		t.Fatalf("untouched worktree produced a patch:\n%s", patch)
	}
	testutil.WriteFile(t, filepath.Join(view, "tracked.txt"), "session edit\n")
	patch := exportAll(t, g, view)
	restored := filepath.Join(t.TempDir(), "restored")
	if err := g.WorktreeAddDetach(ctx, restored, g.BaseOID); err != nil {
		t.Fatal(err)
	}
	if err := g.ApplyPatch(ctx, model.ApplyStrict, restored, g.GitDir, patch); err != nil {
		t.Fatal(err)
	}
	if got, want := fileState(t, restored), fileState(t, view); !reflect.DeepEqual(got, want) {
		t.Fatalf("same-config worktree round trip differs: got %v want %v", got, want)
	}
}
