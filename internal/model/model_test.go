package model

import (
	"errors"
	"testing"
)

func TestWrapNilReturnsNil(t *testing.T) {
	if got := Wrap(ExitApply, "apply patch", nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestPersonaErrorFormattingWithInnerError(t *testing.T) {
	err := &PersonaError{Code: ExitApply, Op: "apply patch", Err: errors.New("boom")}
	if got := err.Error(); got != "apply patch: boom (exit 12)" {
		t.Fatalf("unexpected error string: %q", got)
	}
}

func TestPersonaErrorFormattingWithoutInnerError(t *testing.T) {
	err := &PersonaError{Code: ExitExport, Op: "export patch"}
	if got := err.Error(); got != "export patch (exit 13)" {
		t.Fatalf("unexpected error string: %q", got)
	}
}

func TestPersonaErrorUnwrap(t *testing.T) {
	inner := errors.New("boom")
	err := &PersonaError{Code: ExitWrite, Op: "write patch", Err: inner}
	if !errors.Is(err, inner) {
		t.Fatalf("expected wrapped error %v", inner)
	}
	if got := err.Unwrap(); got != inner {
		t.Fatalf("expected unwrap %v got %v", inner, got)
	}
}

func TestOptionsWithCommand(t *testing.T) {
	cmd := []string{"sh", "-c", "true"}
	opts := Options{PatchPath: "state.patch"}.WithCommand(cmd)
	if opts.PatchPath != "state.patch" {
		t.Fatalf("unexpected patch path: %q", opts.PatchPath)
	}
	if len(opts.Command) != len(cmd) {
		t.Fatalf("unexpected command length: %v", opts.Command)
	}
	cmd[0] = "bash"
	if opts.Command[0] != "bash" {
		t.Fatalf("expected WithCommand to preserve slice aliasing, got %v", opts.Command)
	}
}
