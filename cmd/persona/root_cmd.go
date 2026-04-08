package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"persona/internal/buildinfo"
	"persona/internal/model"

	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	var (
		patchPath      string
		patchDir       string
		printPatchPath bool

		baseMode   string
		baseRef    string
		allowDirty bool

		ignoredMode string
		ignoredMax  int

		applyMode   string
		keepSession string
		verbose     bool
		showVersion bool
	)

	cmd := &cobra.Command{
		Use:           "persona [OPTIONS] -- <command> [args...]",
		Short:         "Run a command in an overlay Git view backed by a patch file",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if showVersion {
				fmt.Fprintln(cmd.OutOrStdout(), buildinfo.PersonaVersion)
				return nil
			}
			if len(args) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "command is required")
				_ = cmd.Help()
				return &exitError{code: model.ExitEnv}
			}
			opts, err := buildOptions(
				patchPath, patchDir, printPatchPath,
				baseMode, baseRef, allowDirty,
				ignoredMode, ignoredMax,
				applyMode, keepSession, verbose,
				args,
			)
			if err != nil {
				return &model.PersonaError{Code: model.ExitEnv, Op: "parse options", Err: err}
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			err, childExit := runWithOptions(ctx, opts)
			if err != nil {
				return err
			}
			if childExit != 0 {
				return &exitError{code: model.ExitCode(childExit)}
			}
			return nil
		},
	}
	cmd.SetOut(os.Stdout)
	cmd.SetErr(os.Stderr)

	cmd.Flags().StringVar(&patchPath, "patch", "", "patch file path (default: auto-generate)")
	cmd.Flags().StringVar(&patchDir, "patch-dir", "", "directory for auto-generated patch files (default: <gitdir>/persona/patches)")
	cmd.Flags().BoolVar(&printPatchPath, "print-patch-path", false, "print patch path on exit")

	cmd.Flags().StringVar(&baseMode, "base-mode", string(model.BaseRepo), "base mode: repo | worktree")
	cmd.Flags().StringVar(&baseRef, "base-ref", "HEAD", "base ref for worktree mode")
	cmd.Flags().BoolVar(&allowDirty, "allow-dirty", false, "allow dirty repo in repo base mode")

	cmd.Flags().StringVar(&ignoredMode, "ignored-mode", string(model.IgnoredTransparent), "ignored mode: transparent | readonly | masked")
	cmd.Flags().IntVar(&ignoredMax, "ignored-max", 200, "max ignored entries to process")

	cmd.Flags().StringVar(&applyMode, "apply-mode", string(model.ApplyStrict), "apply mode: strict | reject")
	cmd.Flags().StringVar(&keepSession, "keep-session", string(model.KeepOnFail), "keep session: on-fail | always | never")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "enable verbose logging")
	cmd.Flags().BoolVar(&showVersion, "version", false, "print the current persona CLI version and exit")

	cmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the current persona CLI version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintln(cmd.OutOrStdout(), buildinfo.PersonaVersion)
		},
	})
	addDiagnosticCommands(cmd)
	addDaemonCommands(cmd)
	return cmd
}

func parseEnum[T ~string](input, name string, valid ...T) (T, error) {
	var zero T
	value := strings.TrimSpace(input)
	for _, item := range valid {
		if value == string(item) {
			return item, nil
		}
	}
	return zero, fmt.Errorf("invalid %s: %s", name, input)
}

func buildOptions(
	patchPath string,
	patchDir string,
	printPatchPath bool,
	baseMode string,
	baseRef string,
	allowDirty bool,
	ignoredMode string,
	ignoredMax int,
	applyMode string,
	keepSession string,
	verbose bool,
	args []string,
) (model.Options, error) {
	var opts model.Options

	opts.PatchPath = strings.TrimSpace(patchPath)
	opts.PatchDir = strings.TrimSpace(patchDir)
	opts.PrintPatchPath = printPatchPath

	mode, err := parseEnum(baseMode, "base-mode", model.BaseRepo, model.BaseWorktree)
	if err != nil {
		return opts, err
	}
	opts.BaseMode = mode
	opts.BaseRef = strings.TrimSpace(baseRef)
	if opts.BaseRef == "" {
		opts.BaseRef = "HEAD"
	}
	if opts.BaseMode == model.BaseRepo && opts.BaseRef != "HEAD" {
		return opts, fmt.Errorf("base-ref is only valid with worktree base-mode")
	}
	opts.AllowDirty = allowDirty

	ignored, err := parseEnum(ignoredMode, "ignored-mode", model.IgnoredTransparent, model.IgnoredReadonly, model.IgnoredMasked)
	if err != nil {
		return opts, err
	}
	if ignoredMax < 0 {
		return opts, fmt.Errorf("ignored-max must be >= 0")
	}
	opts.IgnoredMode = ignored
	opts.IgnoredMax = ignoredMax

	apply, err := parseEnum(applyMode, "apply-mode", model.ApplyStrict, model.ApplyReject)
	if err != nil {
		return opts, err
	}
	opts.ApplyMode = apply

	keep, err := parseEnum(keepSession, "keep-session", model.KeepOnFail, model.KeepAlways, model.KeepNever)
	if err != nil {
		return opts, err
	}
	opts.KeepSession = keep

	opts.Verbose = verbose
	opts.Command = args
	return opts, nil
}
