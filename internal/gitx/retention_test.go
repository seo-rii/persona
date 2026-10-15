package gitx

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"persona/internal/model"
	"persona/internal/testutil"
)

func TestResolveSavedUnbornBase(t *testing.T) {
	isolateGitConfig(t)
	repo := testutil.InitEmptyRepo(t)
	testutil.WriteFile(t, filepath.Join(repo, "staged.bin"), "saved\x00bytes\xff")
	testutil.RunCmd(t, repo, "git", "add", "staged.bin")
	g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	savedOID, err := g.ResolveBase(context.Background(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	patch := exportAll(t, g, repo)
	// The original repository may acquire its first commit before recovery;
	// the persisted empty base must still take precedence over that HEAD.
	testutil.RunCmd(t, repo, "git", "commit", "-m", "first commit after saving the unborn session")
	for _, byField := range []bool{false, true} {
		restarted := &Git{RepoRoot: repo, GitDir: g.GitDir}
		ref := savedOID
		if byField {
			restarted.SetBaseOID(savedOID)
			ref = "HEAD"
		}
		if oid, err := restarted.ResolveBase(context.Background(), ref); err != nil || oid != savedOID {
			t.Fatalf("saved unborn base did not resolve (field=%t): %s %v", byField, oid, err)
		}
		restored := t.TempDir()
		if err := restarted.ApplyPatch(context.Background(), model.ApplyStrict, restored, g.GitDir, patch); err != nil {
			t.Fatalf("saved unborn patch did not restore: %v", err)
		}
		if got, want := fileState(t, restored)["staged.bin"], fileState(t, repo)["staged.bin"]; got != want {
			t.Fatalf("saved unborn contents changed: %s != %s", got, want)
		}
	}
}

func TestRetainBaseSurvivesGarbageCollection(t *testing.T) {
	isolateGitConfig(t)
	repo := testutil.InitRepo(t)
	g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	ctx := context.Background()
	oid, err := g.ResolveBase(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ref := "refs/persona/bases/" + strings.Repeat("a", 64)
	if err := g.RetainBase(ctx, ref, oid); err != nil {
		t.Fatal(err)
	}
	if err := g.RetainBase(ctx, ref, oid); err != nil {
		t.Fatalf("retaining the same base must be idempotent: %v", err)
	}
	tree := gitText(t, repo, "rev-parse", "HEAD^{tree}")
	newRoot := gitText(t, repo, "commit-tree", tree, "-m", "replacement root without the old parent")
	testutil.RunCmd(t, repo, "git", "update-ref", "HEAD", newRoot)
	testutil.RunCmd(t, repo, "git", "reflog", "expire", "--expire=now", "--all")
	testutil.RunCmd(t, repo, "git", "gc", "--prune=now")
	if got := gitText(t, repo, "cat-file", "-t", oid); got != "commit" {
		t.Fatalf("unreferenced session base was pruned: %s", got)
	}
	if err := g.RetainBase(ctx, ref, newRoot); err == nil {
		t.Fatal("retained ref was repointed to a different base")
	}
	if got := gitText(t, repo, "rev-parse", ref); got != oid {
		t.Fatalf("retained base changed: %s -> %s", oid, got)
	}
}

func TestRetainEmptyTreeWritesARealPersistentObject(t *testing.T) {
	isolateGitConfig(t)
	repo := testutil.InitEmptyRepo(t)
	g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	ctx := context.Background()
	oid, err := g.ResolveBase(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ref := "refs/persona/bases/" + strings.Repeat("b", 64)
	if err := g.RetainBase(ctx, ref, oid); err != nil {
		t.Fatalf("retain empty base: %v", err)
	}
	if got := gitText(t, repo, "rev-parse", ref); got != oid {
		t.Fatalf("wrong empty tree ref: %s", got)
	}
	if got := gitText(t, repo, "cat-file", "-t", oid); got != "tree" {
		t.Fatalf("retained empty object is not a tree: %s", got)
	}
	testutil.RunCmd(t, repo, "git", "gc", "--prune=now")
	if err := g.RetainBase(ctx, ref, oid); err != nil {
		t.Fatalf("empty base ref did not survive GC: %v", err)
	}
	restarted := &Git{RepoRoot: repo, GitDir: g.GitDir, BaseOID: oid}
	if got, err := restarted.ResolveBase(ctx, "HEAD"); err != nil || got != oid {
		t.Fatalf("retained empty base did not survive restart: %s %v", got, err)
	}
}

func TestRetainBaseRejectsUnsafeNamesObjectsAndSymbolicRefs(t *testing.T) {
	isolateGitConfig(t)
	repo := testutil.InitRepo(t)
	g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	ctx := context.Background()
	oid := gitText(t, repo, "rev-parse", "HEAD")
	validRef := "refs/persona/bases/" + strings.Repeat("c", 64)
	for _, ref := range []string{"HEAD", "refs/heads/main", "refs/persona/bases/" + strings.Repeat("a", 63), validRef + "0", "refs/persona/bases/" + strings.Repeat("A", 64), validRef + "/child"} {
		if err := g.RetainBase(ctx, ref, oid); err == nil {
			t.Fatalf("unsafe ref name accepted: %q", ref)
		}
	}
	for _, badOID := range []string{"HEAD", "--option", oid[:12], strings.Repeat("0", 40), gitText(t, repo, "rev-parse", "HEAD:tracked.txt")} {
		if err := g.RetainBase(ctx, validRef, badOID); err == nil {
			t.Fatalf("invalid base object accepted: %q", badOID)
		}
	}
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", validRef)
	cmd.Dir = repo
	if err := cmd.Run(); err == nil {
		t.Fatal("rejected operations created a base ref")
	}
	headRef := gitText(t, repo, "symbolic-ref", "HEAD")
	testutil.RunCmd(t, repo, "git", "symbolic-ref", validRef, headRef)
	if err := g.RetainBase(ctx, validRef, oid); err == nil {
		t.Fatal("symbolic retained ref was accepted")
	}
	if got := gitText(t, repo, "rev-parse", "HEAD"); got != oid {
		t.Fatalf("user branch was changed by retention: %s -> %s", oid, got)
	}
}
