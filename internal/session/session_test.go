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
