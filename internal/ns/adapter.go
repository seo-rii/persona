package ns

import "persona/internal/model"

// RealNS is the production NSOps implementation backed by real Linux syscalls.
type RealNS struct{}

var _ model.NSOps = RealNS{}

func (RealNS) UnshareMountNS() error                     { return UnshareMountNS() }
func (RealNS) MakeMountsPrivate() error                   { return MakeMountsPrivate() }
func (RealNS) BindMount(src, dst string) error             { return BindMount(src, dst) }
func (RealNS) RemountRO(target string) error               { return RemountRO(target) }
func (RealNS) Umount(target string) error                  { return Umount(target) }
func (RealNS) MountOverlay(target string, opts model.OverlayOpts) error {
	return MountOverlay(target, OverlayOpts{
		LowerDir: opts.LowerDir,
		UpperDir: opts.UpperDir,
		WorkDir:  opts.WorkDir,
	})
}
func (RealNS) MaskPath(target string, kind model.MaskKind, emptyFile, emptyDir string) error {
	return MaskPath(target, MaskKind(kind), emptyFile, emptyDir)
}
