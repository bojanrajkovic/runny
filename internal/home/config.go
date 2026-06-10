package home

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/goccy/go-yaml"
)

// Config is runnyd's one configuration file (~/.runny/config.yaml). The Zod
// of this repo: parse, default, validate here — nothing downstream re-checks.
type Config struct {
	Pools     []PoolConfig `yaml:"pools"`
	Deadlines Deadlines    `yaml:"deadlines"`
	Limits    Limits       `yaml:"limits"`
	Retention Retention    `yaml:"retention"`
	// NamePrefix prefixes runner names globally: <prefix>-<slot>-<cycle8>.
	NamePrefix string `yaml:"name_prefix"`
}

// GitHubConfig is one pool's App credentials. Each pool carries its own —
// different registration targets are different App installations with
// different keys (ADR-0009).
type GitHubConfig struct {
	AppID          int64  `yaml:"app_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
	// APIBase overrides the GitHub API endpoint (tests, GHES).
	APIBase string `yaml:"api_base"`
}

// PoolConfig is one homogeneous group of runner slots (ADR-0009).
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
	// keys (ADR-0009).
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
}

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

// Deadlines are the per-state budgets of ADR-0004, calibrated from spike
// measurements. Zero values take defaults.
type Deadlines struct {
	Clone     Duration `yaml:"clone"`
	Boot      Duration `yaml:"boot"`
	AwaitIP   Duration `yaml:"await_ip"`
	AwaitSSH  Duration `yaml:"await_ssh"`
	MintJIT   Duration `yaml:"mint_jit"`
	Provision Duration `yaml:"provision"`
	Teardown  Duration `yaml:"teardown"`
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
}

type Retention struct {
	CyclesPerSlot int      `yaml:"cycles_per_slot"`
	MaxAge        Duration `yaml:"max_age"`
}

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
	if c.NamePrefix == "" {
		c.NamePrefix = "runny"
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
		if len(p.Labels) == 0 {
			switch p.OS {
			case "darwin":
				p.Labels = []string{"self-hosted", "macOS", "ARM64"}
			case "linux":
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
	def(&c.Deadlines.PullStall, 3*time.Minute)
	def(&c.Deadlines.Resolve, 60*time.Second)
	def(&c.Limits.MaxJobDuration, 2*time.Hour)
	def(&c.Limits.MaxIdle, 24*time.Hour)
	def(&c.Limits.BackoffBase, 5*time.Second)
	def(&c.Limits.BackoffCap, 5*time.Minute)
	def(&c.Limits.ReconcileInterval, 60*time.Second)
	if c.Retention.CyclesPerSlot == 0 {
		c.Retention.CyclesPerSlot = 50
	}
	if c.Retention.MaxAge == 0 {
		c.Retention.MaxAge = Duration(30 * 24 * time.Hour)
	}
}

var poolNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (c *Config) validate() error {
	var errs []error
	if len(c.Pools) == 0 {
		errs = append(errs, errors.New("at least one pool is required"))
	}
	seen := map[string]bool{}
	for i, p := range c.Pools {
		at := fmt.Sprintf("pools[%d]", i)
		if !poolNameRE.MatchString(p.Name) {
			errs = append(errs, fmt.Errorf("%s: name %q must be lowercase alphanumeric/hyphen", at, p.Name))
		}
		if seen[p.Name] {
			errs = append(errs, fmt.Errorf("%s: duplicate pool name %q", at, p.Name))
		}
		seen[p.Name] = true
		if p.OS != "darwin" && p.OS != "linux" {
			errs = append(errs, fmt.Errorf("%s: os must be darwin or linux, got %q", at, p.OS))
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
		"deadlines.pull_stall":      c.Deadlines.PullStall,
		"deadlines.resolve":         c.Deadlines.Resolve,
		"limits.max_job_duration":   c.Limits.MaxJobDuration,
		"limits.max_idle":           c.Limits.MaxIdle,
		"limits.backoff_base":       c.Limits.BackoffBase,
		"limits.backoff_cap":        c.Limits.BackoffCap,
		"limits.reconcile_interval": c.Limits.ReconcileInterval,
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

	// The Virtualization.framework 2-macOS-guest cap is checked by doctor and
	// startup validation, not here — config parsing stays platform-agnostic.
	return errors.Join(errs...)
}
