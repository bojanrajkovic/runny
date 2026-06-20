// Package sysdaemon installs and removes runnyd as a non-root system
// LaunchDaemon: a dedicated hidden service account, the system home with a dual
// inheriting ACL, the launchd plist, and `launchctl bootstrap system` (#76). It
// is the privileged-once install that lets runnyd RUN unprivileged as a service
// account — strictly better than a root LaunchDaemon. The pure pieces here
// (plist, ACL specs, id allocation, path resolution) are testable without root;
// install.go drives the privileged steps behind a command-runner seam.
package sysdaemon

import (
	"encoding/xml"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/bojanrajkovic/runny/internal/home"
)

// Label is the launchd job label, shared with the per-user agent and the manual
// installer (they differ only by launchd DOMAIN — system/ vs gui/). It MUST stay
// in sync with the per-user plist template, the per-user install.sh, and the
// app's DaemonOwnership.canonicalLabel — Swift and bash can't import this const.
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

// Config is everything the install needs. DefaultConfig fills the fixed parts;
// the caller supplies Operator (the ACL grantee) and RunnydPath.
type Config struct {
	Label        string
	ServiceUser  string
	ServiceGroup string
	HomeDir      string // the system home (home.SystemHomeDir)
	Operator     string // the human operator account the inheriting ACL grants
	RunnydPath   string // absolute path the plist execs
}

// DefaultConfig returns the fixed install parameters; the caller sets Operator
// and RunnydPath.
func DefaultConfig() Config {
	return Config{
		Label:        Label,
		ServiceUser:  ServiceUser,
		ServiceGroup: ServiceGroup,
		HomeDir:      home.SystemHomeDir,
	}
}

// PlistPath is where the system LaunchDaemon plist lives.
func (c Config) PlistPath() string {
	return filepath.Join("/Library/LaunchDaemons", c.Label+".plist")
}

// Plist renders the system LaunchDaemon. Unlike the per-user agent it carries a
// UserName (run as the service account, not the installing root) and a Standard
// ProcessType (a system daemon has no GUI session, so the per-user agent's
// Interactive — which exists to surface the GUI Local Network prompt — is wrong;
// a launchd-started daemon of any uid is auto-allowed local network regardless,
// TN3179). KeepAlive is load-bearing: crash-only teardown and config reload both
// exit non-zero expecting launchd to cold-start the daemon.
func Plist(cfg Config) string {
	logsDir := home.Dir(cfg.HomeDir).LogsDir()
	out := filepath.Join(logsDir, "launchd.out.log")
	errp := filepath.Join(logsDir, "launchd.err.log")
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>

  <!-- Run as the dedicated non-root service account, not the installing root. -->
  <key>UserName</key>
  <string>%s</string>

  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
  </array>

  <key>RunAtLoad</key>
  <true/>

  <!-- KeepAlive is load-bearing for the wedge restart (ADR-0012) and config
       reload (ADR-0014): runnyd exits non-zero expecting a launchd cold start.
       Stop it with "launchctl bootout system/", never by killing it. -->
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>10</integer>

  <!-- Pre-log crash output only; the daemon's structured log rotates under the
       home's logs/runnyd.log. -->
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>

  <!-- Standard, not the per-user agent's Interactive: a system daemon has no
       GUI session, and local network is auto-allowed for launchd daemons. -->
  <key>ProcessType</key>
  <string>Standard</string>
</dict>
</plist>
`, xmlEscape(cfg.Label), xmlEscape(cfg.ServiceUser), xmlEscape(cfg.RunnydPath),
		xmlEscape(out), xmlEscape(errp))
}

// operatorNameRe is the plain-username shape an operator account must match. The
// name is interpolated into the ACL ACE handed to `chmod +a` as a single arg, so
// a name with a space or comma would reshape the ACE into a different (possibly
// broader) grant — reject anything that isn't a bare username.
var operatorNameRe = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$`)

// ValidateOperatorName guards the ACL boundary: the operator account is
// attacker-influenceable only by someone who already holds sudo, but it defines
// a security-critical ACL, so it is validated rather than trusted. The CLI
// additionally checks the account resolves to a real local user.
func ValidateOperatorName(name string) error {
	if !operatorNameRe.MatchString(name) {
		return fmt.Errorf("operator account %q is not a plain username; refusing to build an ACL from it", name)
	}
	return nil
}

// aclInherit makes an ACE apply to the home AND every file/dir created beneath it
// (including the daemon's Ensure() subdirs and operator-landed config/key).
const aclInherit = "file_inherit,directory_inherit"

// operatorACE grants the operator full directory management plus read/write on
// inherited files: edit config, atomically rename over it, land the App key, and
// read the daemon's artifacts. It overrides the home's 0700 POSIX mode (ACL allow
// is evaluated ahead of POSIX). Pinned literal — validated by the PR4c spike.
func operatorACE(operator string) string {
	return "user:" + operator + " allow " +
		"list,add_file,search,delete,add_subdirectory,delete_child," +
		"readattr,writeattr,readextattr,writeextattr,readsecurity," +
		"read,write,append,execute," + aclInherit
}

// serviceACE is the second, load-bearing inheriting ACE: it grants the service
// account READ on inherited files so the daemon can read an operator-LANDED
// config.yaml / .pem (owned by the operator, mode 0600) regardless of the
// operator's umask. Without it the daemon — a different uid, not the file owner —
// cannot read its own config or key (the PR4c spike proved this gap and this fix).
func serviceACE(serviceUser string) string {
	return "user:" + serviceUser + " allow " +
		"list,search,read,readattr,readextattr,readsecurity," + aclInherit
}

// ResolveRunnydPath returns the runnyd the plist should exec: the sibling of the
// running runnyctl. It deliberately does NOT resolve symlinks — Homebrew invokes
// runnyctl via the stable /opt/homebrew/bin symlink, whose sibling runnyd is
// likewise a stable symlink that survives `brew upgrade`; EvalSymlinks would pin
// the versioned Cellar path and orphan the plist on the next upgrade. Verified:
// macOS os.Executable() returns the invocation (symlink) path, not the target.
func ResolveRunnydPath(runnyctlExe string) string {
	return filepath.Join(filepath.Dir(runnyctlExe), "runnyd")
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

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
