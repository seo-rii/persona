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
	paths := []string{s.Root, s.Upper, s.Work, s.MntBase, s.MntGitDir, s.BaseWT, s.Tmp}
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
