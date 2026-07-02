package home

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/goccy/go-yaml"
)

// Config is runnyd's one configuration file (~/.runny/config.yaml). The Zod
// of this repo: parse, default, validate here — nothing downstream re-checks.
type Config struct {
	Pools         []PoolConfig        `yaml:"pools"`
	Deadlines     Deadlines           `yaml:"deadlines"`
	Limits        Limits              `yaml:"limits"`
	Retention     Retention           `yaml:"retention"`
	Observability ObservabilityConfig `yaml:"observability"`
}

// GitHubConfig is one pool's App credentials. Each pool carries its own —
// different registration targets are different App installations with
// different keys.
type GitHubConfig struct {
	AppID          int64  `yaml:"app_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
	// APIBase overrides the GitHub API endpoint (tests, GHES).
	APIBase string `yaml:"api_base"`
}

// PoolConfig is one homogeneous group of runner slots.
type PoolConfig struct {
	// Name becomes the slot prefix: <name>-1, <name>-2, ...
	Name string `yaml:"name"`
	// OS of the guest image: "darwin" or "linux". Declared (not inferred)
	// because the macOS guest-cap check and tarball priming run pre-pull.
	OS    string `yaml:"os"`
	Image string `yaml:"image"`
	Count int    `yaml:"count"`
	// Target is the registration scope: an org, or an owner/repo pair.
	Target TargetConfig `yaml:"target"`
	// GitHub is this pool's App credentials. Required and per-pool: different
	// registration targets are different App installations with different
	// keys.
	GitHub GitHubConfig `yaml:"github"`
	Labels []string     `yaml:"labels"`
	// Runner group; 1 is the default group.
	RunnerGroupID int64 `yaml:"runner_group_id"`
	// Guest credentials; cirruslabs images use admin/admin for both OSes.
	SSHUser     string `yaml:"ssh_user"`
	SSHPassword string `yaml:"ssh_password"`
	// SSHTimeout bounds each SSH operation attempt against this pool's
	// guests: TCP connect, banner+auth, channel open, exec start. Per-pool
	// because guest responsiveness varies by image and load; a guest under
	// teardown pressure may need more headroom than the 3s default.
	SSHTimeout Duration `yaml:"ssh_timeout"`
	// SSHHardening selects what happens to guest SSH after the first
	// authenticated session. "rotate" (the default) mints a
	// per-cycle in-memory keypair, installs it, disables password auth, and
	// reconnects with the key and pinned host keys — the SECURE_SSH state.
	// "off" keeps password auth for the whole cycle (interactive debugging;
	// images whose sshd can't take the drop-in). "scramble" does everything
	// "rotate" does, then additionally randomizes the guest account's
	// password, so the image's well-known default is never reachable again
	// for the rest of the cycle through any channel, not just SSH.
	SSHHardening SSHHardeningMode `yaml:"ssh_hardening"`
	// CPUCores and RAMGB override the guest's CPU count and memory, which
	// otherwise come from the image's baked config.json (e.g. cirruslabs
	// images ship a conservative 2c/4GiB). Zero means "use the image's
	// value". A request below the image's recorded minimum is rejected at
	// boot, not silently clamped. RAMGB is gibibytes.
	CPUCores uint `yaml:"cpu_cores"`
	RAMGB    uint `yaml:"ram_gb"`
}

// SSHHardeningMode is the SSH posture applied to a pool's guests once the
// first password-authenticated session lands. The three values are strictly
// ascending in strictness — off < rotate < scramble, each doing everything
// the one before it does plus one more thing — never independent flags: a
// "scramble but skip rotate" state would leave the known default password
// reachable over still-enabled SSH password auth, which is strictly worse
// than disabling it, so that combination is deliberately unrepresentable
// rather than a validation rule to maintain.
type SSHHardeningMode string

// PoolConfig.SSHHardening values.
const (
	SSHHardeningOff      SSHHardeningMode = "off"
	SSHHardeningRotate   SSHHardeningMode = "rotate"
	SSHHardeningScramble SSHHardeningMode = "scramble"
)

// Scrambles reports whether this mode randomizes the guest account
// password, on top of everything SSHHardeningRotate already does.
//
// There's no analogous Rotates() predicate: the one place that logically
// wants "does this mode rotate" (the SECURE_SSH gate in
// internal/statemachine/fsm.go) deliberately keeps its own
// `!= SSHHardeningOff` comparison instead, because that gate must fail
// CLOSED on a zero-value/undefaulted SSHHardening (rotate rather than
// silently un-harden) — a positive Rotates()-style membership check would
// get that backwards, since the zero value matches neither Rotate nor
// Scramble and would fail OPEN.
func (m SSHHardeningMode) Scrambles() bool {
	return m == SSHHardeningScramble
}

// PoolConfig.OS values. Exported so the config JSON Schema generator
// (tools/configschema) constrains the same set validate() enforces, rather than
// re-typing the literals.
const (
	OSDarwin = "darwin"
	OSLinux  = "linux"
)

// TargetConfig holds exactly one of: Org, or Owner+Repo.
type TargetConfig struct {
	Org   string `yaml:"org"`
	Owner string `yaml:"owner"`
	Repo  string `yaml:"repo"`
}

// IsOrg reports whether the target is org-scoped.
func (t TargetConfig) IsOrg() bool { return t.Org != "" }

func (t TargetConfig) String() string {
	if t.IsOrg() {
		return "org:" + t.Org
	}
	return t.Owner + "/" + t.Repo
}

// Deadlines are the per-state budgets of the crash-only FSM, calibrated from
// spike measurements. Zero values take defaults.
type Deadlines struct {
	Clone     Duration `yaml:"clone"`
	Boot      Duration `yaml:"boot"`
	AwaitIP   Duration `yaml:"await_ip"`
	AwaitSSH  Duration `yaml:"await_ssh"`
	MintJIT   Duration `yaml:"mint_jit"`
	Provision Duration `yaml:"provision"`
	Teardown  Duration `yaml:"teardown"`
	// SecureSSH bounds the per-cycle key rotation (host-key capture, key
	// install + sshd config flip, reconnect): two short execs and
	// a redial against a guest that just answered AWAIT_SSH, so the budget is
	// small; a guest that can't finish in it is wedged, not slow.
	SecureSSH Duration `yaml:"secure_ssh"`
	// PullStall is the progress-based budget for ENSURE_IMAGE: no layer bytes
	// for this long = stuck (a slow pull is expected, a silent one is not).
	PullStall Duration `yaml:"pull_stall"`
	// Resolve bounds the quick metadata round-trips (registry manifest
	// resolve, runner-download resolve) that precede a stall-watched
	// transfer. Shaped by registry/GHES latency, not by runny's internals.
	Resolve Duration `yaml:"resolve"`
}

type Limits struct {
	MaxJobDuration Duration `yaml:"max_job_duration"`
	// MaxIdle recycles a LISTENING runner to absorb image updates.
	MaxIdle     Duration `yaml:"max_idle"`
	BackoffBase Duration `yaml:"backoff_base"`
	BackoffCap  Duration `yaml:"backoff_cap"`
	// ReconcileInterval is the LISTENING-state GitHub registration check.
	ReconcileInterval Duration `yaml:"reconcile_interval"`
	// MaxDebugHold is the default and the cap for a DEBUG hold (runnyctl
	// debug -hold): the auto-release backstop for a forgotten held guest,
	// which occupies a macOS guest-cap slot. For a mid-job injection the
	// clock starts when the job ends, so worst-case slot occupancy is
	// max_job_duration + max_debug_hold. Out-of-range (negative, over-cap)
	// are rejected, not clamped; zero means "default".
	MaxDebugHold Duration `yaml:"max_debug_hold"`
}

type Retention struct {
	CyclesPerSlot int      `yaml:"cycles_per_slot"`
	MaxAge        Duration `yaml:"max_age"`
}

// ObservabilityConfig is the opt-in telemetry block. The zero value (absent
// from config.yaml) means telemetry is fully off: no SDK installed, no
// egress. This is the config surface only — the OTLP emitter that consumes
// it is a separate, later addition.
type ObservabilityConfig struct {
	OTLP OTLPConfig `yaml:"otlp"`
}

// OTLPConfig is the single OTLP export target for both traces and metrics.
// There are deliberately no per-signal endpoints or headers — nothing here
// yet needs them, and they're easy to add later without a breaking change. A
// non-empty Endpoint enables export; its scheme selects transport security.
type OTLPConfig struct {
	// Endpoint is the collector URL. https selects TLS, http selects an
	// insecure connection (for a local collector); any other scheme, an
	// empty host, or a malformed URL, is rejected. Empty (the default) means
	// telemetry is off.
	Endpoint string `yaml:"endpoint"`
	// MetricsInterval is the metrics export period. Zero takes the default;
	// validate() enforces a floor so a typo can't hot-loop the exporter.
	MetricsInterval Duration `yaml:"metrics_interval"`
}

// otlpMetricsIntervalFloor is the minimum observability.otlp.metrics_interval
// — below this, an exporter tick could hot-loop.
const otlpMetricsIntervalFloor = time.Second

// Duration is a time.Duration that unmarshals from YAML strings like "90s".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(b []byte) error {
	var s string
	if err := yaml.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) MarshalYAML() ([]byte, error) {
	return yaml.Marshal(time.Duration(d).String())
}

// LoadConfig reads, defaults, and validates the config file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return parseConfig(raw, path)
}

// LoadConfigSHA loads the config and returns the SHA-256 (hex) of the exact
// bytes it parsed. Reading and hashing once makes the audit hash provably
// describe the validated config: a separate re-read could hash a different
// version under a concurrent atomic replace — the deploy-script workflow a
// reload serves — silently breaking the accept-then-respawn hash chain.
func LoadConfigSHA(path string) (*Config, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("reading config: %w", err)
	}
	// Hash the exact bytes read, before parsing — so the refusal path can
	// still name the rejected file version, while the accept path's hash
	// provably describes what validated (both from this one read).
	sha := fmt.Sprintf("%x", sha256.Sum256(raw))
	cfg, err := parseConfig(raw, path)
	if err != nil {
		return nil, sha, err
	}
	return cfg, sha, nil
}

func parseConfig(raw []byte, path string) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalWithOptions(raw, &c, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	def := func(d *Duration, v time.Duration) {
		if *d == 0 {
			*d = Duration(v)
		}
	}
	for i := range c.Pools {
		p := &c.Pools[i]
		if p.GitHub.APIBase == "" {
			p.GitHub.APIBase = "https://api.github.com"
		}
		if p.Count == 0 {
			p.Count = 1
		}
		if p.RunnerGroupID == 0 {
			p.RunnerGroupID = 1
		}
		if p.SSHUser == "" {
			p.SSHUser = "admin"
		}
		if p.SSHPassword == "" {
			p.SSHPassword = "admin"
		}
		def(&p.SSHTimeout, 3*time.Second)
		if p.SSHHardening == "" {
			p.SSHHardening = SSHHardeningRotate
		}
		if len(p.Labels) == 0 {
			switch p.OS {
			case OSDarwin:
				p.Labels = []string{"self-hosted", "macOS", "ARM64"}
			case OSLinux:
				p.Labels = []string{"self-hosted", "Linux", "ARM64"}
			}
		}
	}
	def(&c.Deadlines.Clone, 10*time.Second)
	def(&c.Deadlines.Boot, 30*time.Second)
	def(&c.Deadlines.AwaitIP, 60*time.Second)
	def(&c.Deadlines.AwaitSSH, 90*time.Second)
	def(&c.Deadlines.MintJIT, 30*time.Second)
	def(&c.Deadlines.Provision, 180*time.Second)
	def(&c.Deadlines.Teardown, 60*time.Second)
	def(&c.Deadlines.SecureSSH, 15*time.Second)
	def(&c.Deadlines.PullStall, 3*time.Minute)
	def(&c.Deadlines.Resolve, 60*time.Second)
	def(&c.Limits.MaxJobDuration, 2*time.Hour)
	def(&c.Limits.MaxIdle, 24*time.Hour)
	def(&c.Limits.BackoffBase, 5*time.Second)
	def(&c.Limits.BackoffCap, 5*time.Minute)
	def(&c.Limits.ReconcileInterval, 60*time.Second)
	def(&c.Limits.MaxDebugHold, 2*time.Hour)
	if c.Retention.CyclesPerSlot == 0 {
		c.Retention.CyclesPerSlot = 50
	}
	if c.Retention.MaxAge == 0 {
		c.Retention.MaxAge = Duration(30 * 24 * time.Hour)
	}
	// Only defaulted when telemetry is actually enabled — an absent endpoint
	// must stay the all-zero, fully-off value.
	if c.Observability.OTLP.Endpoint != "" {
		def(&c.Observability.OTLP.MetricsInterval, 60*time.Second)
	}
}

// PoolNamePattern is the regex a pool name must match: it becomes the slot-name
// prefix, so it must be lowercase/label-safe. Exported so the config JSON Schema
// generator (tools/configschema) constrains the same shape without re-typing it.
const PoolNamePattern = `^[a-z0-9][a-z0-9-]*$`

var poolNameRE = regexp.MustCompile(PoolNamePattern)

func (c *Config) validate() error {
	var errs []error
	if len(c.Pools) == 0 {
		errs = append(errs, errors.New("at least one pool is required"))
	}
	seen := map[string]bool{}
	for i, p := range c.Pools {
		at := fmt.Sprintf("pools[%d]", i)
		// The shape rules below (required keys, the os/ssh_hardening enums, the
		// pool-name pattern) are mirrored in the config JSON Schema. The enums
		// and pattern share the consts/var above; the required-key set is
		// hand-kept in tools/configschema's enrich() — update it when adding a
		// required key.
		if !poolNameRE.MatchString(p.Name) {
			errs = append(errs, fmt.Errorf("%s: name %q must be lowercase alphanumeric/hyphen", at, p.Name))
		}
		if seen[p.Name] {
			errs = append(errs, fmt.Errorf("%s: duplicate pool name %q", at, p.Name))
		}
		seen[p.Name] = true
		if p.OS != OSDarwin && p.OS != OSLinux {
			errs = append(errs, fmt.Errorf("%s: os must be %s or %s, got %q", at, OSDarwin, OSLinux, p.OS))
		}
		if p.Image == "" {
			errs = append(errs, fmt.Errorf("%s: image is required", at))
		}
		if p.Count < 1 {
			errs = append(errs, fmt.Errorf("%s: count must be >= 1", at))
		}
		if p.GitHub.AppID == 0 {
			errs = append(errs, fmt.Errorf("%s: github.app_id is required", at))
		}
		if p.GitHub.PrivateKeyPath == "" {
			errs = append(errs, fmt.Errorf("%s: github.private_key_path is required", at))
		}
		switch p.SSHHardening {
		case SSHHardeningOff, SSHHardeningRotate, SSHHardeningScramble:
		default:
			errs = append(errs, fmt.Errorf("%s: ssh_hardening must be %q, %q, or %q, got %q",
				at, SSHHardeningOff, SSHHardeningRotate, SSHHardeningScramble, p.SSHHardening))
		}
		hasOrg, hasRepo := p.Target.Org != "", p.Target.Owner != "" || p.Target.Repo != ""
		switch {
		case hasOrg && hasRepo:
			errs = append(errs, fmt.Errorf("%s: target must be an org OR owner/repo, not both", at))
		case !hasOrg && (p.Target.Owner == "" || p.Target.Repo == ""):
			errs = append(errs, fmt.Errorf("%s: target needs org, or both owner and repo", at))
		}
	}
	// Durations: defaults have been applied (zero = take the default), so
	// anything non-positive here was set negative explicitly. A negative
	// budget would fail every operation instantly — or, before this check,
	// panicked the stall watcher's ticker.
	for name, d := range map[string]Duration{
		"deadlines.clone":           c.Deadlines.Clone,
		"deadlines.boot":            c.Deadlines.Boot,
		"deadlines.await_ip":        c.Deadlines.AwaitIP,
		"deadlines.await_ssh":       c.Deadlines.AwaitSSH,
		"deadlines.mint_jit":        c.Deadlines.MintJIT,
		"deadlines.provision":       c.Deadlines.Provision,
		"deadlines.teardown":        c.Deadlines.Teardown,
		"deadlines.secure_ssh":      c.Deadlines.SecureSSH,
		"deadlines.pull_stall":      c.Deadlines.PullStall,
		"deadlines.resolve":         c.Deadlines.Resolve,
		"limits.max_job_duration":   c.Limits.MaxJobDuration,
		"limits.max_idle":           c.Limits.MaxIdle,
		"limits.backoff_base":       c.Limits.BackoffBase,
		"limits.backoff_cap":        c.Limits.BackoffCap,
		"limits.reconcile_interval": c.Limits.ReconcileInterval,
		"limits.max_debug_hold":     c.Limits.MaxDebugHold,
		"retention.max_age":         c.Retention.MaxAge,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive, got %v", name, d.D()))
		}
	}
	for i, p := range c.Pools {
		if p.SSHTimeout <= 0 {
			errs = append(errs, fmt.Errorf("pools[%d]: ssh_timeout must be positive, got %v", i, p.SSHTimeout.D()))
		}
	}

	// Separate from the positive-duration map above: MetricsInterval's floor
	// is >= 1s, not merely positive, and — unlike every field in that map —
	// it's validated only when telemetry is on. Empty endpoint means
	// telemetry is off; the rest of the block is only meaningful, and only
	// validated, when it's set.
	if ep := c.Observability.OTLP.Endpoint; ep != "" {
		u, err := url.Parse(ep)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("observability.otlp.endpoint %q: %w", ep, err))
		case u.Hostname() == "":
			// Hostname (not Host) so a port-only "https://:4317" is caught
			// too — its Host is a non-empty ":4317" with nothing to dial.
			// This also catches a bare "host:port" with no "://": url.Parse
			// reads that as an opaque URL with the host packed into Scheme,
			// not as a parse error, so checking the hostname first gives a
			// clearer message than reporting "host:port" back as an invalid
			// scheme.
			errs = append(errs, fmt.Errorf("observability.otlp.endpoint %q: missing host (use https://host:port or http://host:port)", ep))
		case u.Scheme != "https" && u.Scheme != "http":
			errs = append(errs, fmt.Errorf("observability.otlp.endpoint %q: scheme must be https or http, got %q", ep, u.Scheme))
		}
		if c.Observability.OTLP.MetricsInterval.D() < otlpMetricsIntervalFloor {
			errs = append(errs, fmt.Errorf("observability.otlp.metrics_interval must be >= %v, got %v",
				otlpMetricsIntervalFloor, c.Observability.OTLP.MetricsInterval.D()))
		}
	}

	// The Virtualization.framework 2-macOS-guest cap is checked by doctor and
	// startup validation, not here — config parsing stays platform-agnostic.
	return errors.Join(errs...)
}
