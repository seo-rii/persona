package tooling

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"persona/internal/model"
)

func TestTestShRunsCmdAndInternalPackagesAndSkipsWithoutSudo(t *testing.T) {
	repoRoot := repoRoot(t)
	binDir, argsFile := stubGo(t, "dirname")

	cmd := exec.Command("/bin/bash", filepath.Join(repoRoot, "test.sh"))
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"PATH="+binDir,
		"GO_ARGS_FILE="+argsFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test.sh failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "skip integration: sudo not found") {
		t.Fatalf("expected explicit sudo skip message, got %s", out)
	}
	argsData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	if !strings.Contains(string(argsData), "test ./cmd/... ./internal/... -v") {
		t.Fatalf("expected unit go test to cover cmd and internal packages, got %q", string(argsData))
	}
}

func TestTestLogStderrReportsSkippedIntegrationWithoutSudo(t *testing.T) {
	repoRoot := repoRoot(t)
	binDir, argsFile := stubGo(t, "date", "tee", "grep", "rm", "dirname")
	logPath := filepath.Join(t.TempDir(), "persona-test.log")

	cmd := exec.Command("/bin/bash", filepath.Join(repoRoot, "test_log_stderr.sh"), logPath)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"PATH="+binDir,
		"GO_ARGS_FILE="+argsFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test_log_stderr.sh failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "SKIP: sudo not found") {
		t.Fatalf("expected explicit sudo skip message, got %s", out)
	}
	if !strings.Contains(string(out), "integration=SKIP(0)") {
		t.Fatalf("expected skip summary, got %s", out)
	}
	argsData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	if !strings.Contains(string(argsData), "test ./cmd/... ./internal/... -v") {
		t.Fatalf("expected unit go test to cover cmd and internal packages, got %q", string(argsData))
	}
}

func TestBuildShDoesNotRunGoModTidyOnBuildFailure(t *testing.T) {
	repoRoot := repoRoot(t)
	binDir, argsFile := stubGoWithBody(t, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GO_ARGS_FILE\"\nif [ \"$1\" = \"build\" ]; then\n  exit 1\nfi\nif [ \"$1\" = \"mod\" ] && [ \"$2\" = \"tidy\" ]; then\n  exit 0\nfi\nexit 0\n", "dirname", "mkdir", "id")

	cmd := exec.Command("/bin/bash", filepath.Join(repoRoot, "build.sh"))
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"PATH="+binDir,
		"GO_ARGS_FILE="+argsFile,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected build.sh to fail when go build fails\n%s", out)
	}
	argsData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	if !strings.Contains(string(argsData), "build -o") {
		t.Fatalf("expected go build invocation, got %q", string(argsData))
	}
	if strings.Contains(string(argsData), "mod tidy") {
		t.Fatalf("build.sh must not run go mod tidy automatically, got %q", string(argsData))
	}
}

func TestBuildShMentionsCustomOutputDirInFollowUpGuidance(t *testing.T) {
	repoRoot := repoRoot(t)
	binDir, _ := stubGoWithBody(t, "#!/bin/sh\nexit 0\n", "dirname", "mkdir", "id")
	outDir := filepath.Join(t.TempDir(), "out")

	cmd := exec.Command("/bin/bash", filepath.Join(repoRoot, "build.sh"))
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"PATH="+binDir,
		"PERSONA_BUILD_DIR="+outDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build.sh failed: %v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, outDir+"/persona activate") {
		t.Fatalf("expected build.sh guidance to mention custom output dir, got:\n%s", text)
	}
	if strings.Contains(text, "./bin/persona activate") {
		t.Fatalf("expected build.sh guidance to avoid default path when PERSONA_BUILD_DIR is set, got:\n%s", text)
	}
}

func TestReadmeDocumentsCurrentTestShCoverage(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if !strings.Contains(string(data), "`test.sh` runs unit tests (`./cmd/... ./internal/...`) then integration tests.") {
		t.Fatalf("README must document current test.sh unit coverage, got:\n%s", string(data))
	}
}

func TestReadmeDocumentsTestHelperScriptBehavior(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"`test.sh` runs unit tests (`./cmd/... ./internal/...`) then integration tests.",
		"`test_log_stderr.sh` runs unit + integration tests, appends a timestamp to the log filename, writes all output to a single log, and prints a summary at the end.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("README missing helper script contract %q:\n%s", want, text)
		}
	}
}

func TestReadmeUsesRepoRelativeExamplesAndMentionsBuildCapabilities(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "/home/seorii/dev/hancomac/persona") {
		t.Fatalf("README must not contain personal absolute workspace paths")
	}
	if !strings.Contains(text, "`build.sh` builds into `./bin` by default and may try to apply `setcap` (or `sudo setcap`) to the resulting binary.") {
		t.Fatalf("README must describe build.sh capability behavior, got:\n%s", text)
	}
}

func TestReadmeDocumentsCorePersonaFlagsFromHelp(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	readme := string(data)
	help := personaHelp(t, repoRoot)
	for _, flag := range []string{
		"--patch",
		"--patch-dir",
		"--print-patch-path",
		"--base-mode",
		"--base-ref",
		"--allow-dirty",
		"--ignored-mode",
		"--ignored-max",
		"--apply-mode",
		"--keep-session",
		"--verbose",
	} {
		if !strings.Contains(help, flag) {
			t.Fatalf("persona --help missing core flag %s:\n%s", flag, help)
		}
		if !strings.Contains(readme, flag) {
			t.Fatalf("README missing documented core flag %s:\n%s", flag, readme)
		}
	}
}

func TestReadmeDocumentsPersonaUsageFromHelp(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	readme := string(data)
	help := personaHelp(t, repoRoot)
	for _, want := range []string{
		"persona [OPTIONS] -- <command> [args...]",
		"Run a command in an overlay Git view backed by a patch file",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("persona --help missing usage contract %q:\n%s", want, help)
		}
		if !strings.Contains(readme, want) {
			t.Fatalf("README missing usage contract %q:\n%s", want, readme)
		}
	}
}

func TestReadmeDocumentsCorePersonaCommandsFromHelp(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	readme := string(data)
	help := personaHelp(t, repoRoot)
	for _, cmd := range []string{"activate", "doctor"} {
		if !strings.Contains(help, cmd) {
			t.Fatalf("persona --help missing core command %s:\n%s", cmd, help)
		}
		if !strings.Contains(readme, "persona "+cmd) {
			t.Fatalf("README missing documented core command %s:\n%s", cmd, readme)
		}
	}
}

func TestReadmeDocumentsActivateFlagsFromHelp(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	readme := string(data)
	help := personaSubcommandHelp(t, repoRoot, "activate")
	for _, flag := range []string{"--binary", "--allow-dac-override"} {
		if !strings.Contains(help, flag) {
			t.Fatalf("persona activate --help missing flag %s:\n%s", flag, help)
		}
		if !strings.Contains(readme, flag) {
			t.Fatalf("README missing documented activate flag %s:\n%s", flag, readme)
		}
	}
}

func TestReadmeDocumentsDoctorHelpSurface(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	readme := string(data)
	help := personaSubcommandHelp(t, repoRoot, "doctor")
	for _, want := range []string{
		"Check capabilities, mounts, and prerequisites",
		"Usage:",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("persona doctor --help missing %q:\n%s", want, help)
		}
	}
	for _, want := range []string{
		"`persona doctor`: print capability/mount diagnostics and hints for permission issues.",
		"If your binary lives on a `nosuid` mount, file capabilities are ignored; use `sudo` or move the binary.",
	} {
		if !strings.Contains(readme, want) {
			t.Fatalf("README missing doctor guidance %q:\n%s", want, readme)
		}
	}
}

func TestReadmeDocumentsIgnoredDriftOptOut(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	text := string(data)
	want := "If ignored processing is enabled and the ignored path set changes during the run in either direction, export fails."
	if !strings.Contains(text, want) {
		t.Fatalf("README must describe ignored-max opt-out alongside ignored drift failure, got:\n%s", text)
	}
}

func TestReadmeDocumentsBehaviorContracts(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"Patch locking uses a sibling `<patch>.lock` file.",
		"Child commands also run with ambient `GIT_*` variables removed.",
		"Patch files and exported diffs are capped at 16 MiB;",
		"If persona cannot preserve the existing patch file owner/group during atomic write-back, it fails",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("README missing behavior contract %q:\n%s", want, text)
		}
	}
}

func TestReadmeDocumentsExitCodeContract(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"- `0` success",
		"- `10` environment/mount error",
		"- `11` repo state error",
		"- `12` patch apply failure",
		"- `13` export failure",
		"- `14` patch write failure",
		"- Child exit codes propagate unchanged, except child `10`~`14`, which are remapped to `240`~`244`",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("README missing exit code contract %q:\n%s", want, text)
		}
	}
	if int(model.ExitEnv) != 10 || int(model.ExitWrite) != 14 || int(model.ExitChildReservedBase) != 240 {
		t.Fatalf("unexpected exit code constants changed: env=%d write=%d childBase=%d", model.ExitEnv, model.ExitWrite, model.ExitChildReservedBase)
	}
}

func TestReadmeDocumentsPrivilegedIntegrationFlow(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"sudo env \"PATH=$PATH\" PERSONA_INTEGRATION=1 $(command -v go) test ./integration -run TestPersonaIntegration",
		"If `go` is not in the sudo PATH, keep the `env \"PATH=$PATH\"` prefix.",
		"After changes around mount/masking, linked worktree handling, or patch export/apply boundaries, re-run the privileged integration suite",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("README missing privileged integration guidance %q:\n%s", want, text)
		}
	}
}

func TestCIWorkflowUsesDocumentedTestEntrypoints(t *testing.T) {
	repoRoot := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci workflow: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"name: ci",
		"fast:",
		"test:",
		"go test -race ./cmd/... ./internal/...",
		"bash ./test.sh",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("CI workflow missing expected contract %q:\n%s", want, text)
		}
	}
}

func TestReadmeGoVersionMatchesCI(t *testing.T) {
	repoRoot := repoRoot(t)
	readmeData, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	ciData, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci workflow: %v", err)
	}
	readme := string(readmeData)
	ci := string(ciData)
	if !strings.Contains(readme, "Go 1.25+ to build") {
		t.Fatalf("README missing current Go version floor:\n%s", readme)
	}
	if !strings.Contains(ci, "go-version: '1.25.x'") {
		t.Fatalf("CI missing expected Go toolchain line:\n%s", ci)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func personaHelp(t *testing.T, repoRoot string) string {
	t.Helper()
	cmd := exec.Command("go", "run", "./cmd/persona", "--help")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("persona --help failed: %v\n%s", err, out)
	}
	return string(out)
}

func personaSubcommandHelp(t *testing.T, repoRoot string, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"run", "./cmd/persona"}, args...)
	cmdArgs = append(cmdArgs, "--help")
	cmd := exec.Command("go", cmdArgs...)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("persona %s --help failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func stubGo(t *testing.T, extraCommands ...string) (string, string) {
	return stubGoWithBody(t, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GO_ARGS_FILE\"\nexit 0\n", extraCommands...)
}

func stubGoWithBody(t *testing.T, body string, extraCommands ...string) (string, string) {
	t.Helper()
	binDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "go-args.txt")
	script := filepath.Join(binDir, "go")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write go stub: %v", err)
	}
	for _, name := range extraCommands {
		target, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("look up %s: %v", name, err)
		}
		if err := os.Symlink(target, filepath.Join(binDir, name)); err != nil {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}
	return binDir, argsFile
}
