package model

type BaseMode string

type IgnoredMode string

type ApplyMode string

type KeepSessionPolicy string

const (
	BaseRepo     BaseMode = "repo"
	BaseWorktree BaseMode = "worktree"
)

const (
	IgnoredTransparent IgnoredMode = "transparent"
	IgnoredReadonly    IgnoredMode = "readonly"
	IgnoredMasked      IgnoredMode = "masked"
)

const (
	ApplyStrict ApplyMode = "strict"
	ApplyReject ApplyMode = "reject"
)

const (
	KeepOnFail KeepSessionPolicy = "on-fail"
	KeepAlways KeepSessionPolicy = "always"
	KeepNever  KeepSessionPolicy = "never"
)

type Options struct {
	PatchPath      string
	PatchDir       string
	PrintPatchPath bool

	BaseMode   BaseMode
	BaseRef    string
	AllowDirty bool

	IgnoredMode IgnoredMode
	IgnoredMax  int

	ApplyMode   ApplyMode
	KeepSession KeepSessionPolicy
	Verbose     bool

	Command []string
}

func (o Options) WithCommand(cmd []string) Options {
	o.Command = cmd
	return o
}
