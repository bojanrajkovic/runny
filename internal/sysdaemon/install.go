package sysdaemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/bojanrajkovic/runny/internal/home"
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

// Installer performs the privileged install/uninstall. Construct with New; tests
// swap run and writeFile for fakes.
type Installer struct {
	cfg       Config
	run       Runner
	writeFile func(path string, data []byte, perm os.FileMode) error
	log       func(format string, args ...any)
}

// New builds an Installer that shells out for real and logs progress to stdout.
func New(cfg Config) *Installer {
	return &Installer{
		cfg:       cfg,
		run:       execRunner,
		writeFile: os.WriteFile,
		log:       func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
	}
}

// Install is idempotent: reusing an existing service account (so its uid stays
// stable across reinstalls — the home's ownership, which ResolveServer keys on,
// must not drift) and resetting the ACL rather than appending to it.
func (i *Installer) Install(ctx context.Context) error {
	if i.cfg.Operator == "" {
		return fmt.Errorf("operator account is required (it receives the inheriting ACL)")
	}
	if err := ValidateOperatorName(i.cfg.Operator); err != nil {
		return err
	}
	if i.cfg.RunnydPath == "" {
		return fmt.Errorf("runnyd path is required")
	}
	if err := i.ensureAccount(ctx); err != nil {
		return err
	}
	if err := i.ensureHome(ctx); err != nil {
		return err
	}
	if err := i.writePlistFile(); err != nil {
		return err
	}
	return i.bootstrap(ctx)
}

// Uninstall removes the LaunchDaemon but LEAVES the service account and the home
// intact: the home holds config, the App key, and artifacts (a destroy would be
// a footgun), and keeping the account preserves its uid so a reinstall finds the
// home it already owns. Purging account + home is a deliberate manual step.
func (i *Installer) Uninstall(ctx context.Context) error {
	// bootout returns nonzero when the job isn't loaded — not an error for us.
	_, _ = i.run(ctx, "/bin/launchctl", "bootout", "system/"+i.cfg.Label)
	if _, err := i.run(ctx, "/bin/rm", "-f", i.cfg.PlistPath()); err != nil {
		return err
	}
	i.log("removed the system LaunchDaemon (%s); the %s account and %s are left intact",
		i.cfg.Label, i.cfg.ServiceUser, i.cfg.HomeDir)
	return nil
}

func (i *Installer) ensureAccount(ctx context.Context) error {
	// Read UniqueID, not just the record: a half-created account (the bare record
	// exists but attributes failed to land) must NOT count as "exists" — that
	// would skip the rest of the creation and leave a uid-less account the daemon
	// can't run as. Requiring UniqueID means a partial create is repaired, not
	// reused.
	if _, err := i.run(ctx, "/usr/bin/dscl", ".", "-read", "/Users/"+i.cfg.ServiceUser, "UniqueID"); err == nil {
		i.log("service account %s already exists — reusing it", i.cfg.ServiceUser)
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
	grp, usr := "/Groups/"+i.cfg.ServiceGroup, "/Users/"+i.cfg.ServiceUser
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
	i.log("created service account %s (uid %s, gid %s, hidden, no home)", i.cfg.ServiceUser, u, g)
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
	owner := i.cfg.ServiceUser + ":" + i.cfg.ServiceGroup
	logs := home.Dir(i.cfg.HomeDir).LogsDir()
	// Set the home's mode + dual inheriting ACL BEFORE creating logs/, so logs
	// (and every dir the daemon's Ensure() later creates) inherits the ACEs. The
	// ACL is reset (-N) then re-added so a reinstall doesn't stack duplicates.
	steps := [][]string{
		{"/bin/mkdir", "-p", i.cfg.HomeDir},
		{"/usr/sbin/chown", owner, i.cfg.HomeDir},
		{"/bin/chmod", "0700", i.cfg.HomeDir},
		{"/bin/chmod", "-N", i.cfg.HomeDir},
		{"/bin/chmod", "+a", operatorACE(i.cfg.Operator), i.cfg.HomeDir},
		{"/bin/chmod", "+a", serviceACE(i.cfg.ServiceUser), i.cfg.HomeDir},
		{"/bin/mkdir", "-p", logs},
		{"/usr/sbin/chown", owner, logs},
	}
	for _, s := range steps {
		if _, err := i.run(ctx, s[0], s[1:]...); err != nil {
			return err
		}
	}
	i.log("prepared %s (0700, owned by %s, dual inheriting ACL: %s + %s)",
		i.cfg.HomeDir, i.cfg.ServiceUser, i.cfg.Operator, i.cfg.ServiceUser)
	return nil
}

func (i *Installer) writePlistFile() error {
	p := i.cfg.PlistPath()
	if err := i.writeFile(p, []byte(Plist(i.cfg)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", p, err)
	}
	i.log("wrote %s", p)
	return nil
}

func (i *Installer) bootstrap(ctx context.Context) error {
	label := i.cfg.Label
	// bootout a prior generation if one is loaded; nonzero "not loaded" is fine.
	_, _ = i.run(ctx, "/bin/launchctl", "bootout", "system/"+label)
	if _, err := i.run(ctx, "/bin/launchctl", "bootstrap", "system", i.cfg.PlistPath()); err != nil {
		return err
	}
	if _, err := i.run(ctx, "/bin/launchctl", "enable", "system/"+label); err != nil {
		return err
	}
	i.log("bootstrapped %s into system/ (runs as %s)", label, i.cfg.ServiceUser)
	return nil
}
