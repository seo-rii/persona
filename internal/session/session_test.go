package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateAndRemoveAll(t *testing.T) {
	gitDir := t.TempDir()
	s, err := Create(gitDir)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	paths := []string{s.Root, s.Upper, s.Work, s.MntBase, s.BaseWT, s.Tmp}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected path %s: %v", path, err)
		}
	}
	if err := s.RemoveAll(); err != nil {
		t.Fatalf("remove session: %v", err)
	}
	if _, err := os.Stat(s.Root); !os.IsNotExist(err) {
		t.Fatalf("expected session root removed, got %v", err)
	}
}

func TestCreateSessionPathsOwnerOnlyPermissions(t *testing.T) {
	gitDir := t.TempDir()
	s, err := Create(gitDir)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	paths := []struct {
		name string
		path string
	}{
		{name: "root", path: s.Root},
		{name: "upper", path: s.Upper},
		{name: "work", path: s.Work},
		{name: "mnt-base", path: s.MntBase},
		{name: "basewt", path: s.BaseWT},
		{name: "tmp", path: s.Tmp},
	}

	for _, p := range paths {
		info, err := os.Stat(p.path)
		if err != nil {
			t.Fatalf("stat %s: %v", p.name, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("%s expected mode 700, got %o", p.name, got)
		}
	}
}

func TestCreateUsesGitDir(t *testing.T) {
	gitDir := t.TempDir()
	s, err := Create(gitDir)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	expectedPrefix := filepath.Join(gitDir, "persona", "sessions")
	if !strings.HasPrefix(s.Root, expectedPrefix) {
		t.Fatalf("unexpected session root: %s", s.Root)
	}
}

func TestCreateRejectsSymlinkedSessionParent(t *testing.T) {
	gitDir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(gitDir, "persona")); err != nil {
		t.Fatalf("symlink persona dir: %v", err)
	}

	s, err := Create(gitDir)
	if err == nil {
		if s != nil {
			_ = s.RemoveAll()
		}
		t.Fatal("expected symlinked session parent to be rejected")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink error, got %v", err)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatalf("read outside dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no session data outside gitDir, got %d entries", len(entries))
	}
}

func TestSessionRemovalRequiresOwnedDirectChild(t *testing.T) {
	gitDir := filepath.Join(t.TempDir(), ".git")
	s, err := Create(gitDir)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(s.Root), "alias")
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"/", gitDir, filepath.Dir(s.Root), filepath.Dir(filepath.Dir(s.Root)), outside, alias, filepath.Join(s.Root, "upper")} {
		forged := &Session{Root: root, gitDir: gitDir}
		if err := forged.RemoveAll(); err == nil {
			t.Fatalf("accepted unsafe root %q", root)
		}
		if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
			t.Fatalf("outside sentinel lost: %q %v", data, err)
		}
	}
	if _, err := Open(gitDir, s.Root); err != nil {
		t.Fatalf("valid session rejected: %v", err)
	}
	if err := s.RemoveAll(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionMarkerAndExclusiveCreation(t *testing.T) {
	gitDir := filepath.Join(t.TempDir(), ".git")
	first, err := Create(gitDir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Create(gitDir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Root == second.Root {
		t.Fatal("reused session directory")
	}
	if err := os.WriteFile(filepath.Join(first.Root, ownerFile), []byte("forged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.RemoveAll(); err == nil {
		t.Fatal("removed session with mismatched marker")
	}
	if err := second.RemoveAll(); err != nil {
		t.Fatal(err)
	}
}
