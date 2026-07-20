package sysdaemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/launchd"
	"github.com/bojanrajkovic/runny/internal/opacl"
)

// Runner runs a privileged command and returns its combined output. The default
// shells out via exec.CommandContext; tests inject a fake to assert the planned
// command sequence without root.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// commandTimeout bounds every privileged command: dscl (a stuck DirectoryService)
// and launchctl (a wedged launchd) can hang, and no install step may block
// forever — the daemon's whole reason for being is that nothing hangs silently.
const commandTimeout = 30 * time.Second

func execRunner(ctx context.Context, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s",
			name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// verdictTester is a seam for the `-test-config` exec so tests can fake the
// parsed verdict without a real runnyd binary. testconfig.RunTestConfig is the
// production implementation (shared with runnyctl's upgrade and edit-config
// gates).
type verdictTester func(ctx context.Context, binPath, configPath string) (home.Verdict, error)

// Installer performs the privileged install/uninstall. Construct with New; tests
// swap run, writeFile, and testConfig for fakes.
type Installer struct {
	cfg        Config
	run        Runner
	writeFile  func(path string, data []byte, perm os.FileMode) error
	log        func(format string, args ...any)
	plan       *StagePlan // set by WithStage; nil means a bare install (today's crash-loop behavior)
	testConfig verdictTester
}

// WithStage attaches a StagePlan (config + key files staged from an authored
// --config): Install runs stage between ensureHome and bootstrap, and
// bootstraps only once the staged config passes `runnyd -test-config`. A bare
// install (no WithStage call) keeps today's behavior: crash-loop until an
// operator hand-lands config.yaml.
func (i *Installer) WithStage(p StagePlan) *Installer {
	i.plan = &p
	return i
}

// Install is idempotent: reusing an existing service account (so its uid stays
// stable across reinstalls — the home's ownership, which ResolveServer keys on,
// must not drift) and resetting the ACL rather than appending to it.
func (i *Installer) Install(ctx context.Context) error {
	if i.cfg.Operator == "" {
		return fmt.Errorf("operator account is required (it receives the inheriting ACL)")
	}
	if i.cfg.RunnydPath == "" {
		return fmt.Errorf("runnyd path is required")
	}
	u, err := user.Lookup(i.cfg.Operator)
	if err != nil {
		return fmt.Errorf("operator account %q does not resolve to a local user: %w", i.cfg.Operator, err)
	}
	i.cfg.Operator = u.Username
	if err := i.ensureAccount(ctx); err != nil {
		return err
	}
	if err := i.ensureHome(ctx); err != nil {
		return err
	}
	if i.plan != nil {
		// Staging validates before bootstrap: a config that fails `runnyd
		// -test-config` leaves the home scaffolded but NOT started — fix the
		// authored config and rerun install-daemon (idempotent) rather than
		// crash-looping a daemon we already know will refuse it.
		s := stager{runnydPath: i.cfg.RunnydPath, writeOwned: i.writeOwned, log: i.log, testConfig: i.testConfig}
		if err := s.stage(ctx, *i.plan); err != nil {
			return err
		}
	}
	if err := i.writePlistFile(); err != nil {
		return err
	}
	return i.bootstrap(ctx)
}

// writeOwned writes data to path (0600) then chowns it to the operator — the
// file inherits the home's _runny-read ACL by creation, exactly as a hand-cp
// would.
func (i *Installer) writeOwned(ctx context.Context, path string, data []byte) error {
	if err := i.writeFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := i.run(ctx, "/usr/sbin/chown", i.cfg.Operator, path); err != nil {
		return err
	}
	return nil
}

// Uninstall removes the LaunchDaemon AND the home, keeping only the service
// account (so a reinstall reuses its uid and the recreated home's ownership stays
// valid). The home is purged, not preserved: a left-behind home keeps winning
// client resolution (clients pick the system home by existence), so a later
// per-user agent would be unreachable, and it would leave the App key at rest
// after the operator believes runny is gone. bootout is VERIFIED, not assumed —
// its exit status can't distinguish "not loaded" from a real failure (or a hang),
// so the plist and home are removed only after launchctl confirms the job is gone,
// never over a still-running KeepAlive daemon.
func (i *Installer) Uninstall(ctx context.Context) error {
	_, _ = i.run(ctx, "/bin/launchctl", "bootout", "system/"+Label)
	switch loaded, err := i.jobLoaded(ctx); {
	case err != nil:
		return fmt.Errorf("could not confirm %s was unloaded after bootout: %w", Label, err)
	case loaded:
		return fmt.Errorf("%s is still loaded after bootout; refusing to remove the plist and home over a running daemon", Label)
	}
	if _, err := i.run(ctx, "/bin/rm", "-f", PlistPath()); err != nil {
		return err
	}
	if _, err := i.run(ctx, "/bin/rm", "-rf", home.SystemHomeDir); err != nil {
		return err
	}
	i.log("removed the system LaunchDaemon (%s) and %s; the %s account is kept for reinstall",
		Label, home.SystemHomeDir, ServiceUser)
	return nil
}

// jobLoaded reports whether the system job is still registered with launchd,
// via the same launchctl-print tri-state launchd.Classify uses for the
// per-user-agent probe (launchd.go): Registered when loaded, NotRegistered on
// the gone-after-bootout success case, Indeterminate — surfaced to the caller
// rather than swallowed — for anything else (a timeout, a permission denial).
func (i *Installer) jobLoaded(ctx context.Context) (bool, error) {
	out, err := i.run(ctx, "/bin/launchctl", "print", "system/"+Label)
	switch launchd.Classify(out, err) {
	case launchd.Registered:
		return true, nil
	case launchd.NotRegistered:
		return false, nil
	default:
		return false, err
	}
}

func (i *Installer) ensureAccount(ctx context.Context) error {
	// Read the identifying attributes, not just the record. Two reasons: a
	// half-created account (the bare record exists but attributes failed to land)
	// must not count as "exists" and skip the rest of creation; and an account
	// that DOES exist must be verified to be OUR dedicated service account before
	// reuse — a stale or manually-created _runny (a real login shell, a non-system
	// uid, a real home) must never be silently adopted, because the installer
	// would chown the system home and run the daemon as it, handing the App key
	// and socket to the wrong principal.
	out, err := i.run(ctx, "/usr/bin/dscl", ".", "-read", "/Users/"+ServiceUser,
		"UniqueID", "UserShell", "NFSHomeDirectory")
	if err == nil {
		if verr := verifyServiceAccount(out); verr != nil {
			return fmt.Errorf("an account %q already exists but is not the runny service account (%v); "+
				"remove or rename it, then reinstall", ServiceUser, verr)
		}
		i.log("service account %s already exists — reusing it", ServiceUser)
		return nil
	}
	gid, err := i.freeID(ctx, "/Groups", "PrimaryGroupID")
	if err != nil {
		return fmt.Errorf("allocating gid: %w", err)
	}
	uid, err := i.freeID(ctx, "/Users", "UniqueID")
	if err != nil {
		return fmt.Errorf("allocating uid: %w", err)
	}
	g, u := strconv.Itoa(gid), strconv.Itoa(uid)
	grp, usr := "/Groups/"+ServiceGroup, "/Users/"+ServiceUser
	// Hidden (IsHidden), no login shell, home /var/empty (its real home is the
	// system home, which it needs no $HOME to reach).
	steps := [][]string{
		{".", "-create", grp},
		{".", "-create", grp, "PrimaryGroupID", g},
		{".", "-create", grp, "RealName", "Runny Service"},
		{".", "-create", usr},
		{".", "-create", usr, "UniqueID", u},
		{".", "-create", usr, "PrimaryGroupID", g},
		{".", "-create", usr, "UserShell", "/usr/bin/false"},
		{".", "-create", usr, "RealName", "Runny Service"},
		{".", "-create", usr, "NFSHomeDirectory", "/var/empty"},
		{".", "-create", usr, "IsHidden", "1"},
	}
	for _, s := range steps {
		if _, err := i.run(ctx, "/usr/bin/dscl", s...); err != nil {
			return err
		}
	}
	i.log("created service account %s (uid %s, gid %s, hidden, no home)", ServiceUser, u, g)
	return nil
}

func (i *Installer) freeID(ctx context.Context, dsclPath, attr string) (int, error) {
	out, err := i.run(ctx, "/usr/bin/dscl", ".", "-list", dsclPath, attr)
	if err != nil {
		return 0, err
	}
	return firstFreeID(parseTakenIDs(out))
}

func (i *Installer) ensureHome(ctx context.Context) error {
	owner := ServiceUser + ":" + ServiceGroup
	logs := home.Dir(home.SystemHomeDir).LogsDir()
	// Set the home's mode + dual inheriting ACL BEFORE creating logs/, so logs
	// (and every dir the daemon's Ensure() later creates) inherits the ACEs. The
	// ACL ops are RECURSIVE (-R): a changed parent ACL does NOT retroactively
	// inherit onto files that already exist, so on a re-run (e.g. a different
	// operator, or a partial install that lacked the _runny ACE) the existing
	// config/key/artifacts must be re-stamped, not just the root — and a failure
	// to re-stamp surfaces (the run seam returns the error) rather than reporting
	// a success the operator/daemon can't actually use. -N first clears any stale
	// ACL across the tree so the two ACEs don't stack on a reinstall.
	steps := [][]string{
		{"/bin/mkdir", "-p", home.SystemHomeDir},
		{"/usr/sbin/chown", owner, home.SystemHomeDir},
		{"/bin/chmod", "0700", home.SystemHomeDir},
		{"/bin/chmod", "-R", "-N", home.SystemHomeDir},
		{"/bin/chmod", "-R", "+a", opacl.OperatorACE(i.cfg.Operator), home.SystemHomeDir},
		{"/bin/chmod", "-R", "+a", serviceACE(ServiceUser), home.SystemHomeDir},
		{"/bin/mkdir", "-p", logs},
		{"/usr/sbin/chown", owner, logs},
	}
	for _, s := range steps {
		if _, err := i.run(ctx, s[0], s[1:]...); err != nil {
			return err
		}
	}
	i.log("prepared %s (0700, owned by %s, dual inheriting ACL: %s + %s)",
		home.SystemHomeDir, ServiceUser, i.cfg.Operator, ServiceUser)
	return nil
}

func (i *Installer) writePlistFile() error {
	p := PlistPath()
	if err := i.writeFile(p, []byte(Plist(i.cfg)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", p, err)
	}
	i.log("wrote %s", p)
	return nil
}

func (i *Installer) bootstrap(ctx context.Context) error {
	// bootout a prior generation if one is loaded; nonzero "not loaded" is fine.
	_, _ = i.run(ctx, "/bin/launchctl", "bootout", "system/"+Label)
	if _, err := i.run(ctx, "/bin/launchctl", "bootstrap", "system", PlistPath()); err != nil {
		return err
	}
	if _, err := i.run(ctx, "/bin/launchctl", "enable", "system/"+Label); err != nil {
		return err
	}
	i.log("bootstrapped %s into system/ (runs as %s)", Label, ServiceUser)
	return nil
}
