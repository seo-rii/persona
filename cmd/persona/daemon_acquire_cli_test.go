package main

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDaemonAcquireCLIRejectsMissingOrInvalidIdentityBeforeRepoDiscovery(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	probe := filepath.Join(root, "git-called")
	if err := os.WriteFile(filepath.Join(root, "git"), []byte("#!/bin/sh\nprintf called > \"$PERSONA_TEST_GIT_PROBE\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	t.Setenv("PERSONA_TEST_GIT_PROBE", probe)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "missing", want: "required flag(s) \"lease-id\""},
		{name: "empty", args: []string{"--lease-id", ""}, want: "32 lowercase hexadecimal"},
		{name: "short", args: []string{"--lease-id", "abcd"}, want: "32 lowercase hexadecimal"},
		{name: "uppercase", args: []string{"--lease-id", strings.Repeat("AB", 16)}, want: "32 lowercase hexadecimal"},
		{name: "nonhex", args: []string{"--lease-id", strings.Repeat("z", 32)}, want: "32 lowercase hexadecimal"},
		{name: "long", args: []string{"--lease-id", strings.Repeat("ab", 17)}, want: "32 lowercase hexadecimal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(append([]string{"daemon", "acquire", "--session-key", "key", "--owner-pid", strconv.Itoa(os.Getpid())}, tc.args...))
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected lease identity rejection containing %q, got %v", tc.want, err)
			}
			// Repository discovery is the first step on the path to creating a
			// daemon. Invalid identities must be rejected before reaching it.
			if _, err := os.Stat(probe); !os.IsNotExist(err) {
				t.Fatalf("invalid acquisition attempted repository discovery: %v", err)
			}
		})
	}
}
