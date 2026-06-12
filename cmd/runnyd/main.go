// runnyd is the runner daemon: one crash-only state machine per runner slot,
// serving the runny.v1 control surface over a unix socket. Restart is a cold
// start by design (ADR-0004): validate, sweep, run.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bojanrajkovic/runny/internal/bounded"
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

	// Take the single-instance lock before touching the shared log file: a
	// losing second instance must not interleave its startup lines into the
	// winner's structured log. -doctor stays lock-free (read-only, useful
	// against a live daemon).
	if !*checkOnly {
		lock, err := acquireLock(dir.LockPath())
		if err != nil {
			return err
		}
		defer lock.Close()
	}

	// Logging: size-capped file sink + ring buffer, both structured.
	logFile, err := openRotatingFile(dir.LogFile(), logFileCap)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}
	defer logFile.Close()
	ring := logring.NewRing(4096)
	// Runner output gets its own ring: guest run.sh lines, the primary
	// `runnyctl logs` stream, kept apart from the daemon's structured log.
	runnerRing := logring.NewRing(4096)
	logger := slog.New(logring.NewHandler(logFile, slog.LevelDebug, ring))
	slog.SetDefault(logger)
	// config_sha256 chains the audit trail across restarts: a reload logs
	// the hash it validated, and the respawn logs the hash it loaded — same
	// hash, same file, provably.
	logger.Info("runnyd starting", "version", version, "home", dir.String(),
		"config_sha256", configSHA(configPath))

	// Sweep the vms dir BEFORE validation: teardown deliberately retains a
	// wedged guest's clone (ADR-0012), and its divergence can tip
	// disk-headroom under the threshold — validating first would crash-loop
	// every respawn on a leak only this sweep can free. The sweep depends on
	// nothing (not prefix, not clients, not network) and runs only on the
	// real-startup path, under the instance lock — never in -doctor mode,
	// which is read-only and runs alongside a live daemon whose clones must
	// not be deleted.
	if !*checkOnly {
		if err := os.RemoveAll(dir.VMsDir()); err != nil {
			return fmt.Errorf("sweeping vms dir: %w", err)
		}
		if err := dir.Ensure(); err != nil {
			return err
		}
		logger.Info("swept vms dir")
	}

	clients, distinctClients, err := buildClients(cfg)
	if err != nil {
		return err
	}

	doctor := makeDoctor(dir, configPath, cfg, distinctClients)
	if *checkOnly {
		return runDoctor(doctor) // read-only: runs fine alongside a live daemon
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

	// This install's runner-name namespace: <slug(hostname)>-<rand8>, derived
	// and persisted (not configured) so it can't be mistyped and stays stable
	// across restarts — the sweep below depends on it (ADR-0009).
	prefix, err := dir.InstancePrefix()
	if err != nil {
		return err
	}
	if err := home.ValidateRunnerNames(prefix, cfg.Pools); err != nil {
		return err
	}
	logger.Info("runner namespace", "prefix", prefix)

	// Registration sweep: cold start owns the world (ADR-0004). Offline
	// registrations carrying our prefix, on every target. (The vms-dir sweep
	// already ran, before validation.)
	for _, c := range distinctClients {
		sweepRegistrations(ctx, logger, c, prefix)
	}

	// No startup tarball priming: runner tarballs are ensured inside each
	// cycle's ENSURE_IMAGE, where downloads get stall budgets, retries, and
	// why-visibility. Startup network work blocked the socket and once
	// dead-stalled silently — never again.
	var slots []*statemachine.Slot
	for _, p := range cfg.Pools {
		ref, err := oci.ParseRef(p.Image)
		if err != nil {
			return fmt.Errorf("pool %s: %w", p.Name, err)
		}
		gh, osName := clients[ghKey{appID: p.GitHub.AppID, target: p.Target}], p.OS
		deps := statemachine.Deps{
			Home:           dir,
			Config:         cfg,
			Pool:           p,
			InstancePrefix: prefix,
			VM:             vmManager(),
			Images: &images.Ensurer{
				Home: dir,
				Ref:  ref,
				Runner: func(c bounded.Context) (string, string, string, error) {
					return gh.RunnerDownload(c, osName)
				},
				StallBudget:   cfg.Deadlines.PullStall.D(),
				ResolveBudget: cfg.Deadlines.Resolve.D(),
				Log:           logger,
			},
			Clone: func(src tart.Bundle, dst string) error {
				_, err := tart.Clone(src, tart.Bundle(dst))
				return err
			},
			GitHub: gh,
			Dial: guest.Dialer{SSH: sshx.Config{
				User:     p.SSHUser,
				Password: p.SSHPassword,
				Timeout:  p.SSHTimeout.D(),
			}},
			Log: logger,
			OnRunnerLine: func(slot, cycleID, line string) {
				runnerRing.Add(logring.Entry{
					Time: time.Now(), Level: "runner", Message: line,
					Attrs: map[string]string{"slot": slot, "cycle": cycleID},
				})
			},
		}
		for i := 1; i <= p.Count; i++ {
			slots = append(slots, statemachine.NewSlot(fmt.Sprintf("%s-%d", p.Name, i), deps))
		}
	}

	srv := socket.NewServer(slots, ring, runnerRing,
		func(slot string) cycle.Store { return cycle.Store{SlotDir: dir.SlotCyclesDir(slot)} },
		doctor, version)

	// Drain coordination: the wedge escalation (ADR-0012 — a guest that
	// survives force-stop can only be reclaimed by process exit) and the
	// config reload (ADR-0014) share one drainer. It drives every slot to a
	// stable state (wedged, or paused in BACKOFF — running jobs finish
	// first), re-issuing commands on every status change so a dropped
	// command or the backoffWait timer-vs-pause race cannot stall the
	// drain, then exits for a launchd cold start.
	d := &drainer{
		log:  logger,
		stop: stop,
		// The local exit gate, shared by both causes: before handing the
		// process to launchd, prove the on-disk config still parses — the
		// respawn loads it whether the drain was for a wedge or a reload,
		// and holding a drained-but-serving daemon beats a crash-looping
		// socketless one. Local file I/O only: no network work at the exit
		// seam (a refusal there would have no good answer).
		exitGate: func(acceptedSHA string) (bool, string) {
			if _, err := home.LoadConfig(configPath); err != nil {
				return false, fmt.Sprintf("config.yaml no longer parses; the respawn would refuse it: %v", err)
			}
			if sha := configSHA(configPath); acceptedSHA != "" && sha != acceptedSHA {
				logger.Warn("config changed during the drain; the respawn will validate and load the newer file",
					"accepted_sha256", acceptedSHA, "current_sha256", sha)
			}
			return true, ""
		},
	}
	for _, s := range slots {
		d.slots = append(d.slots, s)
		s.OnChange(d.observe)
	}

	// Reload entry point (ADR-0014), shared by the Reload RPC and SIGHUP.
	// Serialized so concurrent callers never run overlapping preflights
	// (each serialized call still revalidates fresh). It never gates on an
	// active drain: the imminent respawn loads the on-disk file whether or
	// not it was validated, so the verdict matters most then. The drain is
	// daemon-owned, never tied to the caller's context — a runnyctl that
	// disconnects after acceptance cannot orphan it.
	var reloadMu sync.Mutex
	requestReload := func(ctx context.Context, source, reason string) socket.ReloadResult {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		logger.Info("config reload requested", "source", source, "reason", reason)
		sha, failed, warnings := preflightReload(ctx, dir, configPath)
		for _, c := range warnings {
			logger.Warn("reload validation warning (not blocking)", "check", c.Name, "detail", c.Detail)
		}
		if len(failed) > 0 {
			for _, c := range failed {
				logger.Error("reload validation failed", "check", c.Name, "detail", c.Detail)
			}
			logger.Error("config reload refused: the new config failed validation; the running daemon is unchanged",
				"failed", len(failed), "config_sha256", sha)
			return socket.ReloadResult{
				FailedChecks: failed,
				Warnings:     warnings,
				Draining:     d.Reason(),
				ConfigSHA256: sha,
			}
		}
		logger.Info("config reload accepted", "config_sha256", sha)
		var pausedSlots []string
		for _, s := range slots {
			if st := s.Status(); st.Paused && !st.Wedged {
				pausedSlots = append(pausedSlots, s.Name())
			}
		}
		if len(pausedSlots) > 0 {
			logger.Warn("operator-paused slots will resume after the respawn (pause is in-memory)",
				"slots", strings.Join(pausedSlots, ", "))
		}
		drainReason := "config reload (" + source + ")"
		if reason != "" {
			drainReason += ": " + reason
		}
		if !d.Start(drainReason, sha) {
			// A drain was already active (wedge, or an earlier reload): the
			// first reason wins on the status surface, but the supplied
			// reason stays durable in the log — the audit trail never loses
			// it.
			logger.Info("reload requested while already draining; supplied reason recorded here",
				"reason", reason, "draining", d.Reason())
		}
		d.recheck() // unblocks a held exit gate when the operator just fixed the file
		return socket.ReloadResult{
			Accepted:            true,
			Warnings:            warnings,
			Draining:            d.Reason(),
			SlotCount:           len(slots),
			OperatorPausedSlots: pausedSlots,
			ConfigSHA256:        sha,
		}
	}
	srv.ReloadFn = func(ctx context.Context, reason string) socket.ReloadResult {
		return requestReload(ctx, "rpc", reason)
	}
	srv.DrainingFn = d.Reason

	// SIGHUP maps to the same validated reload path. A dedicated channel —
	// it must NOT join signal.NotifyContext above, which would cancel the
	// root context. Claiming the signal also disarms its default
	// process-terminate disposition: before this handler, a SIGHUP (or a
	// foreground runnyd's terminal closing) hard-killed runnyd mid-job;
	// now it drains gracefully.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			if d.Reason() != "" {
				// Drain already running: skip the network preflight (storms
				// must not burn token mints + registry resolves per signal);
				// run the cheap local stage only, and scream if it fails —
				// the respawn will load this file.
				if _, err := home.LoadConfig(configPath); err != nil {
					logger.Error("SIGHUP during drain: on-disk config is invalid and the respawn will load it", "err", err)
				}
				d.recheck()
				continue
			}
			_ = requestReload(ctx, "SIGHUP", "")
		}
	}()

	var wg sync.WaitGroup
	for _, s := range slots {
		wg.Go(func() { s.Run(ctx) })
	}
	logger.Info("slots running", "count", len(slots), "socket", dir.SocketPath())

	err = srv.Serve(ctx, dir.SocketPath())
	wg.Wait()
	logger.Info("runnyd stopped")
	if d.Exited() && err == nil {
		// Non-zero exit so launchd (KeepAlive) restarts us — deliberately
		// not a success exit, which a future SuccessfulExit-style plist
		// tweak would leave down silently. The cold start sweeps the vms
		// dir (reclaiming a leaked guest) and loads the on-disk config.
		err = fmt.Errorf("restarting after drain: %s", d.Reason())
	}
	return err
}

// poolClientError attributes a GitHub client construction failure to its
// pool, so the reload preflight can name the failing check.
type poolClientError struct {
	Pool string
	Err  error
}

func (e *poolClientError) Error() string { return fmt.Sprintf("pool %s: %v", e.Pool, e.Err) }
func (e *poolClientError) Unwrap() error { return e.Err }

// buildClients constructs one GitHub client per distinct (App, target):
// pools targeting different orgs or repos usually carry different Apps
// (ADR-0009), so credentials are per-pool. Used by startup and by the
// reload preflight — client construction runs BEFORE the doctor suite at
// startup, so the preflight must replay it too (a deleted private key
// would pass a parse-only preflight and crash-loop the respawn).
func buildClients(cfg *home.Config) (map[ghKey]*github.Client, []*github.Client, error) {
	clients := map[ghKey]*github.Client{}
	var distinct []*github.Client
	for _, p := range cfg.Pools {
		key := ghKey{appID: p.GitHub.AppID, target: p.Target}
		if _, ok := clients[key]; ok {
			continue
		}
		c, err := github.New(github.Config{
			AppID:          p.GitHub.AppID,
			PrivateKeyPath: p.GitHub.PrivateKeyPath,
			APIBase:        p.GitHub.APIBase,
		}, github.Target(p.Target))
		if err != nil {
			return nil, nil, &poolClientError{Pool: p.Name, Err: err}
		}
		clients[key] = c
		distinct = append(distinct, c)
	}
	return clients, distinct, nil
}

// preflightReload runs the full startup gauntlet against the on-disk
// config — config-parse (LoadConfig: strict parse + defaults + validate),
// github-client:<pool> (client construction), then the whole doctor suite
// against the candidate config and clients — so a reload can only be
// accepted when the respawn's own startup validation would pass on the
// same inputs. Each network check self-bounds (checkBudget) regardless of
// ctx; no SSH or guest calls exist on this path (ADR-0011).
func preflightReload(ctx context.Context, dir home.Dir, configPath string) (sha string, failed, warnings []socket.DoctorCheck) {
	sha = configSHA(configPath)
	newCfg, err := home.LoadConfig(configPath)
	if err != nil {
		return sha, []socket.DoctorCheck{{Name: "config-parse", OK: false, Detail: err.Error()}}, nil
	}
	_, newClients, err := buildClients(newCfg)
	if err != nil {
		name := "github-client"
		var pe *poolClientError
		if errors.As(err, &pe) {
			name += ":" + pe.Pool
		}
		return sha, []socket.DoctorCheck{{Name: name, OK: false, Detail: err.Error()}}, nil
	}
	failed, warnings = splitPreflightChecks(makeDoctor(dir, configPath, newCfg, newClients)(ctx))
	return sha, failed, warnings
}

// splitPreflightChecks post-filters the gauntlet's results into the reload
// verdict, adjusting for the preflight running in a different environment
// (guests up) than the respawn (cold):
//
//   - local-network only asserts when a vmnet interface is up — true at
//     reload time, false at the respawn's cold start, where it reports
//     informational green. A flake here must not refuse a reload over a
//     check the respawn cannot fail and no config edit can affect; it
//     becomes a warning.
//   - disk-headroom stays a refusal (fail-safe), but at preflight it counts
//     live clones' divergence that the drain + respawn sweep will free, so
//     the detail says why the number may differ.
func splitPreflightChecks(checks []socket.DoctorCheck) (failed, warnings []socket.DoctorCheck) {
	for _, c := range checks {
		if c.OK {
			continue
		}
		switch c.Name {
		case "local-network":
			warnings = append(warnings, c)
		case "disk-headroom":
			c.Detail += " (measured with guests running; the respawn sweeps clones before re-checking — free space or retry)"
			failed = append(failed, c)
		default:
			failed = append(failed, c)
		}
	}
	return failed, warnings
}

// configSHA is the SHA-256 (hex) of the raw config file bytes — the audit
// handle that proves which file version a reload validated and a cold start
// loaded. Empty when the file is unreadable (the callers' own LoadConfig
// surfaces that loudly).
func configSHA(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
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
	checks := doctor(context.Background()) // each network check self-bounds (checkBudget)
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

// checkBudget bounds each network-touching doctor check individually, no
// matter what context the caller supplies: the Doctor RPC must not rely on
// runnyctl for a bound, and the startup pass runs under the un-deadlined
// signal context — a silent registry must not hang the daemon before the
// socket even exists. Per-check rather than per-suite so one slow target
// cannot starve the remaining checks into false failures.
const checkBudget = 30 * time.Second

// makeDoctor builds the validation suite used at startup and by the Doctor
// RPC: every predecessor failure mode that was checkable but unchecked.
// ghKey dedups github clients: one per distinct (App, registration target).
type ghKey struct {
	appID  int64
	target home.TargetConfig
}

func makeDoctor(dir home.Dir, configPath string, cfg *home.Config, clients []*github.Client) func(context.Context) []socket.DoctorCheck {
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

		// Config drift: behavior comes from the config loaded at startup
		// (cfg), not the file on disk. Post-defaulting struct equality —
		// comment/reorder edits don't false-positive. Trivially green at
		// startup (the file was just loaded), so it can never block daemon
		// entry; under reload validation cfg IS the on-disk candidate, so
		// drift can never block the reload it recommends.
		if onDisk, err := home.LoadConfig(configPath); err != nil {
			add("config-drift", false, err.Error())
		} else if !reflect.DeepEqual(onDisk, cfg) {
			add("config-drift", false, "config.yaml differs from the running config; apply with `runnyctl reload`")
		} else {
			add("config-drift", true, "config.yaml matches the running config")
		}

		// Runner namespace: derives (and persists, first run) the instance
		// prefix — which also proves instance-id is writable, a startup
		// requirement a doctor pass must not paper over — and validates the
		// assembled runner names against GitHub's length cap, which would
		// otherwise surface as a permanent non-retryable 422 loop at MINT_JIT.
		if prefix, err := dir.InstancePrefix(); err != nil {
			add("runner-namespace", false, err.Error())
		} else if err := home.ValidateRunnerNames(prefix, cfg.Pools); err != nil {
			add("runner-namespace", false, err.Error())
		} else {
			add("runner-namespace", true, prefix)
		}

		// The Virtualization.framework concurrent-guest cap applies to macOS
		// guests only; linux pools are bounded by memory, not licensing.
		darwinCount := 0
		for _, p := range cfg.Pools {
			if p.OS == "darwin" {
				darwinCount += p.Count
			}
		}
		if darwinCount <= macOSGuestCap {
			add("macos-guest-cap", true, fmt.Sprintf("%d darwin slot(s) ≤ cap %d", darwinCount, macOSGuestCap))
		} else {
			add("macos-guest-cap", false, fmt.Sprintf(
				"darwin pools total %d slots, exceeding Virtualization.framework's %d-macOS-guest cap; the extra slots could never boot",
				darwinCount, macOSGuestCap,
			))
		}

		// Local Network (TCC): a LaunchDaemon or background-reparented runnyd
		// is silently denied vmnet access, so guest dials fail "no route to
		// host". The probe only asserts once a vmnet interface is up, so at
		// idle startup it is informational; run `runnyd -doctor` (or the
		// Doctor RPC) while a guest is running to confirm the grant. Only
		// meaningful on darwin (docs/deploy.md).
		if runtime.GOOS == "darwin" {
			ok, detail := localNetworkReach()
			add("local-network", ok, detail)
		}

		for _, gh := range clients {
			name := "runner-perm:" + gh.Target().String()
			pctx, cancel := bounded.WithTimeout(ctx, checkBudget)
			err := gh.CheckRunnerPerm(pctx)
			cancel()
			if err != nil {
				add(name, false, err.Error())
			} else {
				add(name, true, "installation token carries the runner-administration permission")
			}
		}

		for _, p := range cfg.Pools {
			name := "image-resolve:" + p.Name
			ref, err := oci.ParseRef(p.Image)
			if err != nil {
				add(name, false, err.Error())
				continue
			}
			rctx, cancel := bounded.WithTimeout(ctx, checkBudget)
			digest, err := oci.NewClient().Resolve(rctx, ref)
			cancel()
			if err != nil {
				add(name, false, err.Error())
			} else {
				add(name, true, fmt.Sprintf("%s → %s", ref, short(digest)))
			}
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

// sweepBudget bounds the whole startup registration sweep. Best-effort by
// design: anything the budget cuts off is retried on the next cold start.
const sweepBudget = time.Minute

func sweepRegistrations(ctx context.Context, log *slog.Logger, gh *github.Client, prefix string) {
	// Self-bounded: this runs under the un-deadlined signal context at
	// startup; a hung GitHub API must not stall daemon boot.
	sctx, cancel := bounded.WithTimeout(ctx, sweepBudget)
	defer cancel()
	runners, err := gh.ListRunners(sctx)
	if err != nil {
		log.Warn("sweep: listing runners failed; stale registrations may linger", "err", err)
		return
	}
	for _, r := range runners {
		if strings.HasPrefix(r.Name, prefix+"-") && r.Status == "offline" {
			if err := gh.DeleteRunner(sctx, r.ID); err != nil {
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

// freeDiskGB: judged by statfs, never du — CoW clones lie to du.
func freeDiskGB(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize) / (1 << 30), nil
}
