package home

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/goccy/go-yaml"
)

// Config is runnyd's one configuration file (~/.runny/config.yaml). The Zod
// of this repo: parse, default, validate here — nothing downstream re-checks.
type Config struct {
	GitHub    GitHubConfig  `yaml:"github"`
	Image     string        `yaml:"image"`
	Runners   RunnersConfig `yaml:"runners"`
	Deadlines Deadlines     `yaml:"deadlines"`
	Limits    Limits        `yaml:"limits"`
	Retention Retention     `yaml:"retention"`
}

type GitHubConfig struct {
	// App authentication (ADR-0003): the App must hold administration:write
	// on the target repo; doctor asserts it on a minted token.
	AppID          int64  `yaml:"app_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
	Owner          string `yaml:"owner"`
	Repo           string `yaml:"repo"`
	// Labels the JIT runners register with.
	Labels []string `yaml:"labels"`
	// Runner group; 1 is the default group for repo-level runners.
	RunnerGroupID int64 `yaml:"runner_group_id"`
	// APIBase overrides the GitHub API endpoint (tests, GHES).
	APIBase string `yaml:"api_base"`
}

type RunnersConfig struct {
	Count      int    `yaml:"count"`
	NamePrefix string `yaml:"name_prefix"`
	// Guest credentials; cirruslabs images use admin/admin.
	SSHUser     string `yaml:"ssh_user"`
	SSHPassword string `yaml:"ssh_password"`
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
	if c.Runners.Count == 0 {
		c.Runners.Count = 2
	}
	if c.Runners.NamePrefix == "" {
		c.Runners.NamePrefix = "runny"
	}
	if c.Runners.SSHUser == "" {
		c.Runners.SSHUser = "admin"
	}
	if c.Runners.SSHPassword == "" {
		c.Runners.SSHPassword = "admin"
	}
	if c.GitHub.RunnerGroupID == 0 {
		c.GitHub.RunnerGroupID = 1
	}
	if c.GitHub.APIBase == "" {
		c.GitHub.APIBase = "https://api.github.com"
	}
	if len(c.GitHub.Labels) == 0 {
		c.GitHub.Labels = []string{"self-hosted", "macOS", "ARM64"}
	}
	def(&c.Deadlines.Clone, 10*time.Second)
	def(&c.Deadlines.Boot, 30*time.Second)
	def(&c.Deadlines.AwaitIP, 60*time.Second)
	def(&c.Deadlines.AwaitSSH, 90*time.Second)
	def(&c.Deadlines.MintJIT, 30*time.Second)
	def(&c.Deadlines.Provision, 180*time.Second)
	def(&c.Deadlines.Teardown, 60*time.Second)
	def(&c.Deadlines.PullStall, 3*time.Minute)
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

func (c *Config) validate() error {
	var errs []error
	if c.GitHub.AppID == 0 {
		errs = append(errs, errors.New("github.app_id is required"))
	}
	if c.GitHub.PrivateKeyPath == "" {
		errs = append(errs, errors.New("github.private_key_path is required"))
	}
	if c.GitHub.Owner == "" || c.GitHub.Repo == "" {
		errs = append(errs, errors.New("github.owner and github.repo are required"))
	}
	if c.Image == "" {
		errs = append(errs, errors.New("image is required"))
	}
	if c.Runners.Count < 1 {
		errs = append(errs, errors.New("runners.count must be >= 1"))
	}
	// The Virtualization.framework 2-macOS-guest cap is checked by doctor and
	// startup validation, not here — config parsing stays platform-agnostic.
	return errors.Join(errs...)
}
