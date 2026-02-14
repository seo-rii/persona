package main

import (
	"path/filepath"
	"testing"
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
