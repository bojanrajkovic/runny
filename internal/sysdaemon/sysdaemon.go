// Package sysdaemon installs and removes runnyd as a non-root system
// LaunchDaemon: a dedicated hidden service account, the system home with a dual
// home ACL, the launchd plist, and `launchctl bootstrap system`. It
// is the privileged-once install that lets runnyd RUN unprivileged as a service
// account — strictly better than a root LaunchDaemon. The pure pieces here
// (plist, ACL specs, id allocation, path resolution) are testable without root;
// install.go drives the privileged steps behind a command-runner seam.
package sysdaemon

import (
	"fmt"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"howett.net/plist"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/opacl"
)

// Label is the launchd job label, shared with the app's per-user LaunchAgent
// (they differ only by launchd DOMAIN — system/ vs gui/). It MUST stay in sync
// with the app's bundled LaunchAgent plist and its DaemonOwnership.canonicalLabel
// — Swift can't import this const.
const Label = "com.coderinserepeat.runnyd"

// ServiceUser/ServiceGroup is the dedicated, hidden, home-less account runnyd
// runs as. Its entire state lives in home.SystemHomeDir, so it needs no home
// directory of its own (NFSHomeDirectory is /var/empty).
const (
	ServiceUser  = "_runny"
	ServiceGroup = "_runny"
)

// The service account's uid/gid is auto-allocated from the system range rather
// than pinned: a fixed id would be byte-identical across a fleet but risks
// colliding with an account a host already has. macOS reserves <500 for system
// accounts; 200–400 is the conventional service-account band.
const (
	idRangeLo = 200
	idRangeHi = 400
)

// Config is what varies per install: the ACL grantee and the binary path.
// Everything else (Label, ServiceUser, ServiceGroup, the system home) is fixed
// and lives in package consts, used directly rather than threaded through here.
type Config struct {
	Operator   string // the human operator account the home directory's ACL grants
	RunnydPath string // absolute path the plist execs
}

// validate checks Operator/RunnydPath are set and Operator resolves to a
// local user, canonicalizing it to that user's username — shared by every
// platform's Install so the rule and its wording can't drift between them.
func (c *Config) validate() error {
	if c.Operator == "" {
		return fmt.Errorf("operator account is required (it receives write access to the system home)")
	}
	if c.RunnydPath == "" {
		return fmt.Errorf("runnyd path is required")
	}
	u, err := user.Lookup(c.Operator)
	if err != nil {
		return fmt.Errorf("operator account %q does not resolve to a local user: %w", c.Operator, err)
	}
	c.Operator = u.Username
	return nil
}

// PlistPath is where the system LaunchDaemon plist lives.
func PlistPath() string {
	return filepath.Join("/Library/LaunchDaemons", Label+".plist")
}

// launchDaemonPlist is the typed shape of the system LaunchDaemon plist.
// Field order controls key order in the marshaled XML.
type launchDaemonPlist struct {
	Label string `plist:"Label"`
	// Run as the dedicated non-root service account, not the installing root.
	UserName         string   `plist:"UserName"`
	ProgramArguments []string `plist:"ProgramArguments"`
	RunAtLoad        bool     `plist:"RunAtLoad"`
	// KeepAlive is load-bearing for wedge escalation and config reload: runnyd
	// exits non-zero expecting a launchd cold start. Stop it with
	// "launchctl bootout system/", never by killing it.
	KeepAlive         bool   `plist:"KeepAlive"`
	ThrottleInterval  int    `plist:"ThrottleInterval"`
	StandardOutPath   string `plist:"StandardOutPath"`
	StandardErrorPath string `plist:"StandardErrorPath"`
	// Standard, not the per-user agent's Interactive: a system daemon has no GUI
	// session, and launchd-started daemons of any uid are auto-allowed local
	// network (TN3179).
	ProcessType string `plist:"ProcessType"`
}

// Plist renders the system LaunchDaemon. Unlike the per-user agent it carries a
// UserName (run as the service account, not the installing root) and a Standard
// ProcessType (a system daemon has no GUI session, so the per-user agent's
// Interactive — which exists to surface the GUI Local Network prompt — is wrong;
// a launchd-started daemon of any uid is auto-allowed local network regardless,
// TN3179). KeepAlive is load-bearing: wedge escalation and config reload both
// exit non-zero expecting launchd to cold-start the daemon.
func Plist(cfg Config) string {
	logsDir := home.Dir(home.SystemHomeDir).LogsDir()
	p := launchDaemonPlist{
		Label:             Label,
		UserName:          ServiceUser,
		ProgramArguments:  []string{cfg.RunnydPath},
		RunAtLoad:         true,
		KeepAlive:         true,
		ThrottleInterval:  10,
		StandardOutPath:   filepath.Join(logsDir, "launchd.out.log"),
		StandardErrorPath: filepath.Join(logsDir, "launchd.err.log"),
		ProcessType:       "Standard",
	}
	out, err := plist.Marshal(p, plist.XMLFormat)
	if err != nil {
		// Marshal of a well-typed struct with known-good types cannot fail.
		panic(fmt.Sprintf("sysdaemon: plist.Marshal: %v", err))
	}
	return string(out)
}

// serviceACE is the second, load-bearing inheriting ACE: it grants the service
// account READ on inherited files so the daemon can read an operator-LANDED
// config.yaml / .pem (owned by the operator, mode 0600) regardless of the
// operator's umask. Without it the daemon — a different uid, not the file owner —
// cannot read its own config or key (the PR4c spike proved this gap and this fix).
func serviceACE(serviceUser string) string {
	return "user:" + serviceUser + " allow " +
		"list,search,read,readattr,readextattr,readsecurity," + opacl.ACLInherit
}

// ResolveRunnydPath returns the runnyd the plist/service should exec: the
// sibling of the running runnyctl. It deliberately does NOT resolve symlinks —
// Homebrew invokes runnyctl via the stable /opt/homebrew/bin symlink, whose
// sibling runnyd is likewise a stable symlink that survives `brew upgrade`;
// EvalSymlinks would pin the versioned Cellar path and orphan the plist on the
// next upgrade. Verified: macOS os.Executable() returns the invocation
// (symlink) path, not the target.
func ResolveRunnydPath(runnyctlExe string) string {
	return resolveRunnydPath(runnyctlExe, runtime.GOOS)
}

// resolveRunnydPath is ResolveRunnydPath's GOOS decision as a small pure
// helper, kept separate so the windows ".exe" case is unit-tested cross-host
// without a windows build.
func resolveRunnydPath(runnyctlExe, goos string) string {
	name := "runnyd"
	if goos == "windows" {
		name = "runnyd.exe"
	}
	return filepath.Join(filepath.Dir(runnyctlExe), name)
}

// firstFreeID returns the lowest id in the service range not present in taken.
func firstFreeID(taken map[int]bool) (int, error) {
	for id := idRangeLo; id <= idRangeHi; id++ {
		if !taken[id] {
			return id, nil
		}
	}
	return 0, fmt.Errorf("no free uid/gid in the %d-%d service range", idRangeLo, idRangeHi)
}

// verifyServiceAccount checks a `dscl -read /Users/<svc> UniqueID UserShell
// NFSHomeDirectory` dump describes OUR dedicated service account — a no-login
// shell, the /var/empty home, and a system-range uid — so a pre-existing foreign
// or login account under the same name is refused rather than silently adopted
// (the installer would otherwise chown the system home and run the daemon as it).
func verifyServiceAccount(dsclRead string) error {
	attrs := map[string]string{}
	for _, line := range strings.Split(dsclRead, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			attrs[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if sh := attrs["UserShell"]; sh != "/usr/bin/false" {
		return fmt.Errorf("UserShell is %q, not /usr/bin/false (a login account)", sh)
	}
	if h := attrs["NFSHomeDirectory"]; h != "/var/empty" {
		return fmt.Errorf("home is %q, not /var/empty", h)
	}
	uid, err := strconv.Atoi(attrs["UniqueID"])
	if err != nil {
		return fmt.Errorf("unreadable UniqueID %q", attrs["UniqueID"])
	}
	if uid <= 0 || uid >= 500 {
		return fmt.Errorf("UniqueID %d is not a system service uid (>=500 is a login user)", uid)
	}
	return nil
}

// parseTakenIDs reads the id column of `dscl . -list /<Users|Groups> <attr>`
// output ("recordname   123") into a set, skipping unparseable lines.
func parseTakenIDs(dsclList string) map[int]bool {
	taken := map[int]bool{}
	for _, line := range strings.Split(dsclList, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if id, err := strconv.Atoi(f[len(f)-1]); err == nil {
			taken[id] = true
		}
	}
	return taken
}
