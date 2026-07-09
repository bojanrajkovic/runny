package home

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
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
	// GuestEnv is a set of environment variables exported into the guest shell
	// immediately before the runner launches, so run.sh and every job step
	// inherit them (e.g. HTTPS_PROXY for a pool whose jobs must reach services
	// through a host-side proxy). Absent means inject nothing — provisioning is
	// byte-identical to a pool without it. Keys must be POSIX environment-variable
	// names (guestEnvNameRE), enforced here at load; values are
	// single-quote-escaped into the provision script and never evaluated, the
	// same trust-boundary discipline the runner-tarball name gets. It is not for
	// secrets: the values land in the guest's process args during provisioning.
	GuestEnv map[string]string `yaml:"guest_env"`
	// GuestSetup is an ordered list of shell commands run in the guest as
	// admin (passwordless sudo available), after the guest_env exports and
	// before the runner launches — for system-level setup guest_env can't
	// express (e.g. macOS's system proxy, which CFNetwork/Xcode read instead
	// of *_proxy env vars). Absent means run nothing — provisioning is
	// byte-identical to a pool without it. Entries are operator-authored,
	// trusted config, injected verbatim into the provision script; their
	// content can't be meaningfully validated, so only emptiness is checked
	// at load. Like guest_env, this is not for secrets: entries are visible in
	// the guest's process args during provisioning.
	GuestSetup []string `yaml:"guest_setup"`
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

// Deadlines are the per-state budgets of the slot FSM, calibrated from
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

// durationField is one row of a config-field table: the config key name, an
// accessor onto the Duration field it defaults/validates, and its default
// value. Rooted at *Config so one row shape covers fields under any
// sub-struct (Deadlines, Limits, Retention).
type durationField struct {
	name string
	get  func(*Config) *Duration
	def  time.Duration
}

// deadlineFields is the single source of truth for every deadlines.* field —
// applyDefaults, validate, and Warnings' floor check all iterate this same
// table, so adding a deadline is a one-row change instead of three edits that
// can silently drift apart.
var deadlineFields = []durationField{
	{"deadlines.clone", func(c *Config) *Duration { return &c.Deadlines.Clone }, 10 * time.Second},
	{"deadlines.boot", func(c *Config) *Duration { return &c.Deadlines.Boot }, 30 * time.Second},
	{"deadlines.await_ip", func(c *Config) *Duration { return &c.Deadlines.AwaitIP }, 60 * time.Second},
	{"deadlines.await_ssh", func(c *Config) *Duration { return &c.Deadlines.AwaitSSH }, 90 * time.Second},
	{"deadlines.mint_jit", func(c *Config) *Duration { return &c.Deadlines.MintJIT }, 30 * time.Second},
	{"deadlines.provision", func(c *Config) *Duration { return &c.Deadlines.Provision }, 180 * time.Second},
	{"deadlines.teardown", func(c *Config) *Duration { return &c.Deadlines.Teardown }, 60 * time.Second},
	{"deadlines.secure_ssh", func(c *Config) *Duration { return &c.Deadlines.SecureSSH }, 15 * time.Second},
	{"deadlines.pull_stall", func(c *Config) *Duration { return &c.Deadlines.PullStall }, 3 * time.Minute},
	{"deadlines.resolve", func(c *Config) *Duration { return &c.Deadlines.Resolve }, 60 * time.Second},
}

// limitsFields is deadlineFields' sibling table for limits.* and
// retention.max_age: same one-row-covers-default-and-validate deal, kept
// separate from deadlineFields because Warnings' floor check is
// deliberately deadline-only (limits are much larger budgets, not
// guest-op-latency bounds) and must not iterate these.
var limitsFields = []durationField{
	{"limits.max_job_duration", func(c *Config) *Duration { return &c.Limits.MaxJobDuration }, 2 * time.Hour},
	{"limits.max_idle", func(c *Config) *Duration { return &c.Limits.MaxIdle }, 24 * time.Hour},
	{"limits.backoff_base", func(c *Config) *Duration { return &c.Limits.BackoffBase }, 5 * time.Second},
	{"limits.backoff_cap", func(c *Config) *Duration { return &c.Limits.BackoffCap }, 5 * time.Minute},
	{"limits.reconcile_interval", func(c *Config) *Duration { return &c.Limits.ReconcileInterval }, 60 * time.Second},
	{"limits.max_debug_hold", func(c *Config) *Duration { return &c.Limits.MaxDebugHold }, 2 * time.Hour},
	{"retention.max_age", func(c *Config) *Duration { return &c.Retention.MaxAge }, 30 * 24 * time.Hour},
}

// allDurationFields is deadlineFields+limitsFields combined: applyDefaults and
// validate's positive check treat every row identically (default it, then
// require positive), so they iterate this one list. Only Warnings' floor
// check cares about the deadline/limits distinction and iterates
// deadlineFields directly instead. Built via append into a nil slice so it
// gets its own backing array, never aliasing either source table's.
var allDurationFields = append(append([]durationField(nil), deadlineFields...), limitsFields...)

type Limits struct {
	MaxJobDuration Duration `yaml:"max_job_duration"`
	// MaxIdle recycles a LISTENING runner to absorb image updates.
	MaxIdle     Duration `yaml:"max_idle"`
	BackoffBase Duration `yaml:"backoff_base"`
	BackoffCap  Duration `yaml:"backoff_cap"`
	// ReconcileInterval is the LISTENING-state GitHub registration check.
	ReconcileInterval Duration `yaml:"reconcile_interval"`
	// MaxDebugHold is the default and the cap for a DEBUG hold (runnyctl
	// debug --hold): the auto-release backstop for a forgotten held guest,
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
// Endpoint and Headers deliberately apply to both signals — nothing here
// yet needs per-signal values, and they're easy to add later without a
// breaking change. A non-empty Endpoint enables export; its scheme selects
// transport security.
type OTLPConfig struct {
	// Endpoint is the collector URL. https selects TLS, http selects an
	// insecure connection (for a local collector); any other scheme, an
	// empty host, or a malformed URL, is rejected. Empty (the default) means
	// telemetry is off.
	Endpoint string `yaml:"endpoint"`
	// Headers are extra key/value pairs sent with every OTLP export request,
	// for both signals. This is how OTLP backends authenticate — e.g.
	// x-honeycomb-team, api-key, or authorization: Bearer <token>. Values may
	// reference environment variables with the Collector's ${env:VAR} syntax,
	// resolved once at load; an unset variable is a load error, never a
	// silently empty header. Every other byte, including bare $, passes
	// through untouched — secrets legitimately contain dollar signs.
	Headers map[string]string `yaml:"headers"`
	// MetricsInterval is the metrics export period. Zero takes the default;
	// validate() enforces a floor so a typo can't hot-loop the exporter.
	MetricsInterval Duration `yaml:"metrics_interval"`
}

// Enabled reports whether this config turns telemetry on — the one
// definition of "is telemetry configured" every caller (validation, the
// OTLP runtime, the trace consumer's wiring) shares, so they can't drift.
func (c OTLPConfig) Enabled() bool { return c.Endpoint != "" }

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
	cfg, _, err := LoadConfigSHA(path)
	return cfg, err
}

// ParseConfig defaults and validates already-read config bytes. path is used
// only for error messages. For a caller that also needs the raw bytes
// themselves (e.g. to rewrite them, or to hash them), this is one read
// shared between both uses — re-reading the file a second time for the parse
// risks parsing a different version than the one the caller already has in
// hand (the same hazard LoadConfigSHA reads once to avoid).
func ParseConfig(raw []byte, path string) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalWithOptions(raw, &c, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	c.applyDefaults()
	// validate() reads no header values, so it can run (and join errors)
	// even when expansion failed.
	if err := errors.Join(c.expandOTLPHeaders(), c.validate()); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &c, nil
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
	cfg, err := ParseConfig(raw, path)
	if err != nil {
		return nil, sha, err
	}
	return cfg, sha, nil
}

// envPlaceholderRE is the Collector's ${env:VAR} placeholder syntax. Only
// this exact shape expands; shell-style $VAR or ${VAR} deliberately does
// not, so a literal $ inside a secret value survives.
var envPlaceholderRE = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandOTLPHeaders resolves ${env:VAR} placeholders in OTLP header values.
// A disabled block (empty endpoint) is left alone: its headers are dead
// config, and expanding them could fail a boot that sends no telemetry.
func (c *Config) expandOTLPHeaders() error {
	if !c.Observability.OTLP.Enabled() {
		return nil
	}
	var errs []error
	for k, v := range c.Observability.OTLP.Headers {
		c.Observability.OTLP.Headers[k] = envPlaceholderRE.ReplaceAllStringFunc(v, func(m string) string {
			name := m[len("${env:") : len(m)-1]
			val, ok := os.LookupEnv(name)
			if !ok {
				errs = append(errs, fmt.Errorf("observability.otlp.headers[%q]: environment variable %s is not set", k, name))
			}
			return val
		})
	}
	return errors.Join(errs...)
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
	for _, f := range allDurationFields {
		def(f.get(c), f.def)
	}
	if c.Retention.CyclesPerSlot == 0 {
		c.Retention.CyclesPerSlot = 50
	}
	// Only defaulted when telemetry is actually enabled — an absent endpoint
	// must stay the all-zero, fully-off value.
	if c.Observability.OTLP.Enabled() {
		def(&c.Observability.OTLP.MetricsInterval, 60*time.Second)
	}
}

// PoolNamePattern is the regex a pool name must match: it becomes the slot-name
// prefix, so it must be lowercase/label-safe. Exported so the config JSON Schema
// generator (tools/configschema) constrains the same shape without re-typing it.
const PoolNamePattern = `^[a-z0-9][a-z0-9-]*$`

var poolNameRE = regexp.MustCompile(PoolNamePattern)

// guestEnvNameRE is the shape a pools[].guest_env key must match: a POSIX
// environment-variable name. A key is exported verbatim into the guest's shell
// (guest.guestEnvExports), so a name that isn't a valid identifier would be a
// broken `export` at best; validate() rejects it here, at load, rather than
// letting it fail on the guest at provision time.
var guestEnvNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// positiveDuration returns a "%s must be positive" error when d<=0, else nil
// — the shared floor every duration field in this config uses except
// observability.otlp.metrics_interval, which has its own, higher floor and
// is checked inline where the rest of OTLP's validation already lives. A nil
// return composes directly into errs via append, same as every other check
// in validate() (errors.Join drops the nils).
func positiveDuration(name string, d Duration) error {
	if d <= 0 {
		return fmt.Errorf("%s must be positive, got %v", name, d.D())
	}
	return nil
}

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
		for k := range p.GuestEnv {
			if !guestEnvNameRE.MatchString(k) {
				errs = append(errs, fmt.Errorf("%s: guest_env key %q is not a valid environment variable name (%s)", at, k, guestEnvNameRE.String()))
			}
		}
		for i, cmd := range p.GuestSetup {
			if strings.TrimSpace(cmd) == "" {
				errs = append(errs, fmt.Errorf("%s: guest_setup[%d] must not be empty or whitespace-only", at, i))
			}
		}
	}
	// Durations: defaults have been applied (zero = take the default), so
	// anything non-positive here was set negative explicitly. A negative
	// budget would fail every operation instantly — or, before this check,
	// panicked the stall watcher's ticker.
	for _, f := range allDurationFields {
		errs = append(errs, positiveDuration(f.name, *f.get(c)))
	}
	for i, p := range c.Pools {
		errs = append(errs, positiveDuration(fmt.Sprintf("pools[%d]: ssh_timeout", i), p.SSHTimeout))
	}
	// Not a Duration (a cycle count), so it isn't in allDurationFields, but the
	// same silent-wrong-policy risk applies: Store.Prune's count-based branch
	// is guarded by `keepCount > 0`, so a negative value doesn't error or
	// crash there — it quietly disables count-based retention and falls back
	// to age-only pruning, a materially different policy than configured.
	if c.Retention.CyclesPerSlot <= 0 {
		errs = append(errs, fmt.Errorf("retention.cycles_per_slot must be positive, got %d", c.Retention.CyclesPerSlot))
	}

	// Separate from the positive-duration check above: MetricsInterval's floor
	// is >= 1s, not merely positive, and — unlike every field checked above —
	// it's validated only when telemetry is on. Empty endpoint means
	// telemetry is off; the rest of the block is only meaningful, and only
	// validated, when it's set.
	if ep := c.Observability.OTLP.Endpoint; c.Observability.OTLP.Enabled() {
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
		for name := range c.Observability.OTLP.Headers {
			if name == "" {
				errs = append(errs, errors.New("observability.otlp.headers: header name must not be empty"))
			}
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
