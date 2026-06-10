// runnyd is the runner daemon: one crash-only state machine per runner slot,
// serving the runny.v1 control surface over a unix socket. Restart is a cold
// start by design (ADR-0004): validate, sweep, run.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bojanrajkovic/runny/internal/cycle"
	"github.com/bojanrajkovic/runny/internal/github"
	"github.com/bojanrajkovic/runny/internal/guest"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/images"
	"github.com/bojanrajkovic/runny/internal/logring"
	"github.com/bojanrajkovic/runny/internal/oci"
	"github.com/bojanrajkovic/runny/internal/socket"
	"github.com/bojanrajkovic/runny/internal/sshx"
	"github.com/bojanrajkovic/runny/internal/statemachine"
	"github.com/bojanrajkovic/runny/internal/tart"
)

var version = "dev"

// macOSGuestCap: Virtualization.framework hard-caps concurrent macOS guests.
// The predecessor accepted configs over the cap and churned forever; we
// refuse to start (pillar 7).
const macOSGuestCap = 2

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "runnyd:", err)
		os.Exit(1)
	}
}

func run() error {
	homeFlag := flag.String("home", "", "runny home dir (default $RUNNY_HOME or ~/.runny)")
	configFlag := flag.String("config", "", "config path (default <home>/config.yaml)")
	checkOnly := flag.Bool("doctor", false, "run validation checks and exit")
	flag.Parse()

	dir, err := home.Resolve(*homeFlag)
	if err != nil {
		return err
	}
	if err := dir.Ensure(); err != nil {
		return err
	}
	configPath := *configFlag
	if configPath == "" {
		configPath = dir.ConfigPath()
	}
	cfg, err := home.LoadConfig(configPath)
	if err != nil {
		return err
	}

	// Logging: file sink + ring buffer, both structured.
	logFile, err := os.OpenFile(dir.LogFile(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}
	defer logFile.Close()
	ring := logring.NewRing(4096)
	logger := slog.New(logring.NewHandler(logFile, slog.LevelDebug, ring))
	slog.SetDefault(logger)
	logger.Info("runnyd starting", "version", version, "home", dir.String())

	gh, err := github.New(github.Config{
		AppID:          cfg.GitHub.AppID,
		PrivateKeyPath: cfg.GitHub.PrivateKeyPath,
		Owner:          cfg.GitHub.Owner,
		Repo:           cfg.GitHub.Repo,
		APIBase:        cfg.GitHub.APIBase,
	})
	if err != nil {
		return err
	}
	ref, err := oci.ParseRef(cfg.Image)
	if err != nil {
		return err
	}

	doctor := makeDoctor(dir, cfg, gh, ref)
	if *checkOnly {
		return runDoctor(doctor)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Startup validation: fail loudly now, not silently later.
	if failed := failedChecks(doctor(ctx)); len(failed) > 0 {
		for _, c := range failed {
			logger.Error("startup check failed", "check", c.Name, "detail", c.Detail)
		}
		return fmt.Errorf("%d startup check(s) failed (run runnyd -doctor for detail)", len(failed))
	}

	// Sweep: cold start owns the world (ADR-0004). Clones first, then any
	// offline registrations carrying our prefix.
	if err := os.RemoveAll(dir.VMsDir()); err != nil {
		return fmt.Errorf("sweeping vms dir: %w", err)
	}
	if err := dir.Ensure(); err != nil {
		return err
	}
	sweepRegistrations(ctx, logger, gh, cfg.Runners.NamePrefix)

	// Runner tarball cache (shared into guests via virtiofs).
	if _, err := images.EnsureRunnerTarball(ctx, dir.RunnerCacheDir()); err != nil {
		return fmt.Errorf("priming runner cache: %w", err)
	}

	deps := statemachine.Deps{
		Home:   dir,
		Config: cfg,
		VM:     vmManager(),
		Images: &images.Ensurer{Home: dir, Ref: ref, StallBudget: cfg.Deadlines.PullStall.D(), Log: logger},
		Clone: func(src tart.Bundle, dst string) error {
			_, err := tart.Clone(src, tart.Bundle(dst))
			return err
		},
		GitHub: gh,
		Dial: guest.Dialer{SSH: sshx.Config{
			User:     cfg.Runners.SSHUser,
			Password: cfg.Runners.SSHPassword,
			Timeout:  3 * time.Second,
		}},
		Log: logger,
	}

	slots := make([]*statemachine.Slot, cfg.Runners.Count)
	for i := range slots {
		slots[i] = statemachine.NewSlot(fmt.Sprintf("runner-%d", i+1), deps)
	}

	srv := socket.NewServer(slots, ring,
		func(slot string) cycle.Store { return cycle.Store{SlotDir: dir.SlotCyclesDir(slot)} },
		doctor, version)

	var wg sync.WaitGroup
	for _, s := range slots {
		wg.Go(func() { s.Run(ctx) })
	}
	logger.Info("slots running", "count", len(slots), "socket", dir.SocketPath())

	err = srv.Serve(ctx, dir.SocketPath())
	wg.Wait()
	logger.Info("runnyd stopped")
	return err
}

// failedChecks filters to failures.
func failedChecks(checks []socket.DoctorCheck) []socket.DoctorCheck {
	var out []socket.DoctorCheck
	for _, c := range checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

func runDoctor(doctor func(context.Context) []socket.DoctorCheck) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	checks := doctor(ctx)
	bad := 0
	for _, c := range checks {
		mark := "ok"
		if !c.OK {
			mark, bad = "FAIL", bad+1
		}
		fmt.Printf("%-28s %-4s %s\n", c.Name, mark, c.Detail)
	}
	if bad > 0 {
		return fmt.Errorf("%d check(s) failed", bad)
	}
	return nil
}

// makeDoctor builds the validation suite used at startup and by the Doctor
// RPC: every predecessor failure mode that was checkable but unchecked.
func makeDoctor(dir home.Dir, cfg *home.Config, gh *github.Client, ref oci.Ref) func(context.Context) []socket.DoctorCheck {
	return func(ctx context.Context) []socket.DoctorCheck {
		var checks []socket.DoctorCheck
		add := func(name string, ok bool, detail string) {
			checks = append(checks, socket.DoctorCheck{Name: name, OK: ok, Detail: detail})
		}

		if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
			add("platform", true, "darwin/arm64")
		} else {
			add("platform", false, fmt.Sprintf("%s/%s — VMs require darwin/arm64", runtime.GOOS, runtime.GOARCH))
		}

		if cfg.Runners.Count <= macOSGuestCap {
			add("runner-count", true, fmt.Sprintf("%d ≤ cap %d", cfg.Runners.Count, macOSGuestCap))
		} else {
			add("runner-count", false, fmt.Sprintf(
				"runners.count=%d exceeds Virtualization.framework's %d-macOS-guest cap; the extra slots could never boot",
				cfg.Runners.Count, macOSGuestCap))
		}

		if err := gh.CheckAdminWrite(ctx); err != nil {
			add("github-admin-write", false, err.Error())
		} else {
			add("github-admin-write", true, "installation token carries administration:write")
		}

		if digest, err := oci.NewClient().Resolve(ctx, ref); err != nil {
			add("image-resolve", false, err.Error())
		} else {
			add("image-resolve", true, fmt.Sprintf("%s → %s", ref, short(digest)))
		}

		free, err := freeDiskGB(dir.String())
		switch {
		case err != nil:
			add("disk-headroom", false, err.Error())
		case free < 30:
			// Judged by df, never du — CoW clones lie to du (image economics).
			add("disk-headroom", false, fmt.Sprintf("%dGB free; <30GB risks mid-job disk exhaustion", free))
		default:
			add("disk-headroom", true, fmt.Sprintf("%dGB free", free))
		}

		return checks
	}
}

func sweepRegistrations(ctx context.Context, log *slog.Logger, gh *github.Client, prefix string) {
	runners, err := gh.ListRunners(ctx)
	if err != nil {
		log.Warn("sweep: listing runners failed; stale registrations may linger", "err", err)
		return
	}
	for _, r := range runners {
		if strings.HasPrefix(r.Name, prefix+"-") && r.Status == "offline" {
			if err := gh.DeleteRunner(ctx, r.ID); err != nil {
				log.Warn("sweep: deleting stale runner", "name", r.Name, "err", err)
			} else {
				log.Info("sweep: removed stale registration", "name", r.Name)
			}
		}
	}
}

func short(digest string) string {
	if len(digest) > 19 {
		return digest[:19]
	}
	return digest
}

var errNotDarwin = errors.New("vm boot requires darwin/arm64 (see -doctor)")
