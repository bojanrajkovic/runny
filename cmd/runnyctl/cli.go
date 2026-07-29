package main

import (
	"context"
	"fmt"
	"time"

	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// CLI is the runnyctl command grammar; kong fills it from the argv. The global
// --json is honored in any position (before or after the subcommand). Each
// command's Run method is a thin adapter onto a ctl.* implementation — the RPC
// and rendering logic lives there, unchanged. A dialing command's Run takes
// (*ctl, context.Context), both bound in run(); the two privileged local
// commands (install/uninstall) take neither and never dial.
//
// SLOT arguments accept the bare slot name (mac-1) or a full runner name as
// shown by status and the GitHub runners page (<prefix>-mac-1-<cycle>).
type CLI struct {
	JSON bool `help:"emit protojson instead of human rendering"`

	Version         VersionCmd         `cmd:"" help:"print the client version"`
	Status          StatusCmd          `cmd:"" help:"one-shot slot status"`
	Watch           WatchCmd           `cmd:"" help:"follow status transitions"`
	Logs            LogsCmd            `cmd:"" help:"stream runner output (all slots, or just SLOT)"`
	Recycle         RecycleCmd         `cmd:"" help:"destroy SLOT's current cycle and start fresh"`
	Debug           DebugCmd           `cmd:"" help:"inject a public key into SLOT's live guest and hold it in DEBUG"`
	Pause           PauseCmd           `cmd:"" help:"hold SLOT after its current cycle drains"`
	Resume          ResumeCmd          `cmd:"" help:"release a paused SLOT"`
	Reload          ReloadCmd          `cmd:"" help:"validate the on-disk config; if valid, drain the fleet and restart runnyd on it"`
	UpgradeDaemon   UpgradeDaemonCmd   `cmd:"" name:"upgrade-daemon" help:"validate the in-place config with the on-disk runnyd, then drain-gated reload onto it"`
	Why             WhyCmd             `cmd:"" help:"render SLOT's recent cycle timelines"`
	Doctor          DoctorCmd          `cmd:"" help:"run the daemon's validation checks"`
	Prune           PruneCmd           `cmd:"" help:"show (or reclaim) stale image bundles and runner tarballs"`
	Operator        OperatorCmd        `cmd:"" help:"grant, revoke, or list operators"`
	EditConfig      EditConfigCmd      `cmd:"" name:"edit-config" help:"edit the resolved home's config.yaml, validate it, then reload"`
	InstallDaemon   InstallDaemonCmd   `cmd:"" name:"install-daemon" help:"install runnyd as an unprivileged system service (LaunchDaemon on macOS, SCM service on Windows; requires root/elevation)"`
	UninstallDaemon UninstallDaemonCmd `cmd:"" name:"uninstall-daemon" help:"remove the system service AND its home (config, key, artifacts)"`
	Image           ImageCmd           `cmd:"" help:"build tart-format OCI images"`
}

type VersionCmd struct{}

// Run prints the client version. It is the one command that must work without a
// daemon, so it makes no RPC (warnSkew is also skipped for it in run()).
func (VersionCmd) Run(c *ctl) error {
	fmt.Fprintln(c.out, version)
	return nil
}

type StatusCmd struct{}

func (StatusCmd) Run(c *ctl, ctx context.Context) error { return c.status(ctx) }

type WatchCmd struct{}

func (WatchCmd) Run(c *ctl, ctx context.Context) error { return c.watch(ctx) }

type LogsCmd struct {
	Slot   string `arg:"" optional:"" help:"limit to one slot (or runner name); omit for all slots"`
	Replay int    `default:"50" help:"buffered lines to replay"`
	Follow bool   `default:"true" negatable:"" help:"keep following after the replay"`
	Daemon bool   `help:"stream the daemon's own log instead of runner output"`
}

func (l *LogsCmd) Run(c *ctl, ctx context.Context) error {
	// SLOT is optional here, so the at-most-one rule is kong's (single arg field);
	// --daemon streams the daemon log and so cannot be combined with a slot filter.
	if l.Daemon && l.Slot != "" {
		return fmt.Errorf("--daemon and a slot filter are mutually exclusive")
	}
	return c.logs(ctx, l.Replay, l.Follow, l.Daemon, l.Slot)
}

type RecycleCmd struct {
	Slot   string `arg:"" help:"slot or runner name"`
	Reason string `default:"operator request" help:"reason recorded in the cycle"`
	Force  bool   `help:"recycle a DEBUG hold, or cancel a RUNNING job"`
}

func (r *RecycleCmd) Run(c *ctl, ctx context.Context) error {
	return c.recycle(ctx, r.Slot, r.Reason, r.Force)
}

type DebugCmd struct {
	Slot   string        `arg:"" help:"slot or runner name"`
	Pubkey string        `help:"public key file (default ~/.ssh/id_ed25519.pub)"`
	Hold   time.Duration `help:"auto-release after this long (0 = limits.max_debug_hold)"`
	Reason string        `help:"audit note"`
	NoExec bool          `help:"print connect info without exec'ing into ssh"`
}

func (d *DebugCmd) Run(c *ctl, ctx context.Context) error {
	return c.debug(ctx, d.Slot, d.Pubkey, d.Hold, d.Reason, d.NoExec)
}

type PauseCmd struct {
	Slot string `arg:"" help:"slot or runner name"`
}

func (p *PauseCmd) Run(c *ctl, ctx context.Context) error { return c.pause(ctx, p.Slot) }

type ResumeCmd struct {
	Slot string `arg:"" help:"slot or runner name"`
}

func (r *ResumeCmd) Run(c *ctl, ctx context.Context) error {
	_, err := c.client.Resume(ctx, &runnyv1.ResumeRequest{Slot: r.Slot})
	if err == nil {
		fmt.Fprintf(c.out, "%s resumed\n", r.Slot)
	}
	return err
}

type ReloadCmd struct {
	Reason         string        `help:"reason recorded in the daemon log and cycle records"`
	Wait           bool          `help:"follow the drain and confirm the respawn came up on this config"`
	RespawnTimeout time.Duration `default:"90s" help:"max wait for the respawn after the daemon exits"`
	Timeout        time.Duration `help:"optional hard cap on the entire wait (0 = none)"`
}

func (r *ReloadCmd) Run(c *ctl, ctx context.Context) error {
	if r.Wait {
		return c.reloadWait(ctx, r.Reason, c.plainReload, defaultFollowOpts(r.RespawnTimeout, r.Timeout))
	}
	return c.reload(ctx, r.Reason)
}

type UpgradeDaemonCmd struct {
	Force          bool          `help:"upgrade despite config warnings (never overrides a hard error)"`
	RespawnTimeout time.Duration `default:"90s" help:"max wait for the respawn after the daemon exits"`
	Timeout        time.Duration `help:"optional hard cap on the entire wait (0 = none)"`
}

func (u *UpgradeDaemonCmd) Run(c *ctl, ctx context.Context) error {
	return c.upgradeDaemon(ctx, u.Force, defaultFollowOpts(u.RespawnTimeout, u.Timeout))
}

type WhyCmd struct {
	Slot   string `arg:"" help:"slot or runner name"`
	Cycles int    `default:"1" help:"how many recent cycles"`
}

func (w *WhyCmd) Run(c *ctl, ctx context.Context) error { return c.why(ctx, w.Slot, w.Cycles) }

type DoctorCmd struct{}

func (DoctorCmd) Run(c *ctl, ctx context.Context) error { return c.doctor(ctx) }

type PruneCmd struct {
	Apply bool `help:"delete the reclaimable items (default: dry run)"`
}

func (p *PruneCmd) Run(c *ctl, ctx context.Context) error { return c.prune(ctx, p.Apply) }

// OperatorCmd is runnyctl's one command group — grant/revoke/list are one
// cohesive noun, unlike the otherwise-flat command set.
type OperatorCmd struct {
	Grant  OperatorGrantCmd  `cmd:"" help:"grant USER (name or uid) operator status"`
	Revoke OperatorRevokeCmd `cmd:"" help:"revoke USER's operator status"`
	List   OperatorListCmd   `cmd:"" help:"list granted operators, with who granted them and when"`
}

type OperatorGrantCmd struct {
	User string `arg:"" help:"user name or uid"`
}

func (o *OperatorGrantCmd) Run(c *ctl, ctx context.Context) error {
	return c.operatorGrant(ctx, o.User)
}

type OperatorRevokeCmd struct {
	User string `arg:"" help:"user name or uid"`
}

func (o *OperatorRevokeCmd) Run(c *ctl, ctx context.Context) error {
	return c.operatorRevoke(ctx, o.User)
}

type OperatorListCmd struct{}

func (OperatorListCmd) Run(c *ctl, ctx context.Context) error { return c.operatorList(ctx) }

type EditConfigCmd struct{}

func (EditConfigCmd) Run(c *ctl, ctx context.Context) error { return c.editConfig(ctx) }

type InstallDaemonCmd struct {
	Operator string `help:"operator account the home directory's ACL grants (defaults to $SUDO_USER; required when run as root without sudo)"`
	Config   string `help:"stage this config.yaml (and the keys its pools reference) into the home and validate before starting the daemon"`
}

func (i *InstallDaemonCmd) Run() error { return installDaemon(i.Operator, i.Config) }

type UninstallDaemonCmd struct{}

func (UninstallDaemonCmd) Run() error { return uninstallDaemon() }

// ImageCmd is runnyctl's other command group (OperatorCmd above is the
// first) — pack is the one verb today, but "build tart-format OCI images"
// is a cohesive noun the same way operator grant/revoke/list is.
type ImageCmd struct {
	Pack ImagePackCmd `cmd:"" help:"pack a disk image into a tart-format OCI Image Layout directory"`
}

// ImagePackCmd never dials the daemon — see main.go's early-return switch —
// so its Run takes neither *ctl nor a context.Context, matching
// InstallDaemonCmd/UninstallDaemonCmd above.
//
// OS deliberately excludes "darwin" (a Codex review on this PR caught it):
// tart.Bundle.LoadConfig requires hardwareModel/ecid for a darwin config,
// this command has no flags to supply either, and WriteImage's own
// validation (see pack.go) means a darwin pack would always fail, not
// sometimes -- so kong refuses it up front with a clear error instead of
// advertising a guest OS this command can never actually produce. Darwin
// support (accepting or generating that metadata) is a separate, later
// change, not a narrower version of this one.
type ImagePackCmd struct {
	Disk       string `arg:"" name:"disk" help:"path to the disk image (raw or VHDX -- packed through unchanged)"`
	OCILayout  string `name:"oci-layout" default:"./out" help:"output OCI Image Layout directory"`
	OS         string `required:"" enum:"linux,windows" help:"guest OS (linux or windows; darwin isn't supported by this command yet)"`
	Arch       string `required:"" help:"guest architecture (arm64 or amd64)"`
	CPUCount   uint   `name:"cpu-count" required:"" help:"guest vCPU count"`
	MemorySize uint64 `name:"memory-size" required:"" help:"guest memory size in bytes"`
	NVRAM      string `name:"nvram" help:"path to nvram bytes to embed (windows: default is a minimal placeholder, HCS never reads it; darwin/linux: required, VZ parses this file as real firmware state)"`
}

func (p *ImagePackCmd) Run() error {
	return imagePack(p.Disk, p.OCILayout, p.OS, p.Arch, p.CPUCount, p.MemorySize, p.NVRAM)
}
