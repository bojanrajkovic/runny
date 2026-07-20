//go:build windows

package sysdaemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/bojanrajkovic/runny/internal/home"
)

// scmService is the subset of *mgr.Service scmInstaller needs. Its method set
// is spelled to match mgr.Service exactly, so the real type satisfies it with
// no adapter; windows-tagged tests fake it to assert call order without a live
// SCM.
type scmService interface {
	UpdateConfig(c mgr.Config) error
	SetRecoveryActions(actions []mgr.RecoveryAction, resetPeriod uint32) error
	SetRecoveryActionsOnNonCrashFailures(flag bool) error
	Start(args ...string) error
	Control(c svc.Cmd) (svc.Status, error)
	Query() (svc.Status, error)
	Delete() error
	Close() error
}

// scmMgr is the subset of *mgr.Mgr scmInstaller needs, returning scmService
// instead of *mgr.Service so a fake never needs a real SCM handle. Unlike
// scmService this needs a thin real-side adapter (realMgr below) — Go's
// interface satisfaction isn't covariant on return types.
type scmMgr interface {
	OpenService(name string) (scmService, error)
	CreateService(name, exepath string, c mgr.Config) (scmService, error)
	Disconnect() error
}

type realMgr struct{ m *mgr.Mgr }

func (r realMgr) OpenService(name string) (scmService, error) {
	s, err := r.m.OpenService(name)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (r realMgr) CreateService(name, exepath string, c mgr.Config) (scmService, error) {
	s, err := r.m.CreateService(name, exepath, c)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (r realMgr) Disconnect() error { return r.m.Disconnect() }

// connectSCM is the production connect seam over golang.org/x/sys/windows/svc/mgr.
func connectSCM() (scmMgr, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, err
	}
	return realMgr{m}, nil
}

// scmInstaller is the Windows sibling of Installer: same method-set contract
// (WithStage, Install, Uninstall) so cmd/runnyctl/installdaemon.go compiles
// unchanged, but service lifecycle goes through the typed mgr seam (connect)
// instead of Runner text commands — icacls is the one exception, staying on
// run, the same seam darwin uses for chmod/dscl.
type scmInstaller struct {
	cfg        Config
	connect    func() (scmMgr, error) // connectSCM in New, faked in tests
	run        Runner
	writeFile  func(path string, data []byte, perm os.FileMode) error
	mkdirAll   func(path string, perm os.FileMode) error // ensureHome; os.MkdirAll in New, faked in tests
	removeAll  func(path string) error                   // Uninstall; os.RemoveAll in New, faked in tests
	log        func(format string, args ...any)
	plan       *StagePlan
	testConfig verdictTester
}

// WithStage mirrors Installer.WithStage.
func (s *scmInstaller) WithStage(p StagePlan) *scmInstaller {
	s.plan = &p
	return s
}

// writeOwned writes 0600 and does NOT chown — unlike darwin, operator access
// arrives via the home's inherited grant ACE (icacls, ensureHome), not file
// ownership.
func (s *scmInstaller) writeOwned(ctx context.Context, path string, data []byte) error {
	return s.writeFile(path, data, 0o600)
}

// Install ensures the runnyd service exists (before home ownership — see
// ensureService's doc comment), configures crash/non-crash recovery, prepares
// the home's ACL, stages a provided config, then (re)starts the service.
func (s *scmInstaller) Install(ctx context.Context) error {
	if err := s.cfg.validate(); err != nil {
		return err
	}

	m, err := s.connect()
	if err != nil {
		return fmt.Errorf("connecting to the service control manager: %w", err)
	}
	defer m.Disconnect()

	svcHandle, err := s.ensureService(m)
	if err != nil {
		return err
	}
	defer svcHandle.Close()

	if err := svcHandle.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 10 * time.Second}}, 86400); err != nil {
		return fmt.Errorf("configuring recovery actions: %w", err)
	}
	if err := svcHandle.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("configuring non-crash recovery: %w", err)
	}

	if err := s.ensureHome(ctx); err != nil {
		return err
	}
	if s.plan != nil {
		st := stager{runnydPath: s.cfg.RunnydPath, writeOwned: s.writeOwned, log: s.log, testConfig: s.testConfig}
		if err := st.stage(ctx, *s.plan); err != nil {
			return err
		}
	}
	// Stop before start, mirroring darwin's bootstrap (install.go) always
	// booting out a prior generation before bootstrapping fresh: StartService
	// rejects an already-running service with ERROR_SERVICE_ALREADY_RUNNING,
	// and an idempotent reinstall's updated binary path only takes effect on
	// the next start — a no-op over a running service would silently leave it
	// on the old binary/config.
	if err := s.stopIfRunning(ctx, svcHandle); err != nil {
		return fmt.Errorf("stopping %s before restart: %w", WindowsServiceName, err)
	}
	if err := svcHandle.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", WindowsServiceName, err)
	}
	s.log("installed and started %s (runs as %s)", WindowsServiceName, windowsServiceSID)
	return nil
}

// ensureService reuses an existing service (idempotent reinstall — updating
// only its binary path) or creates one. LookupSID("NT SERVICE\runnyd") does
// not resolve before the service is registered, so this runs BEFORE
// ensureHome: nothing can reference the service SID in an icacls grant until
// this returns.
func (s *scmInstaller) ensureService(m scmMgr) (scmService, error) {
	cfg := mgr.Config{
		ServiceStartName: windowsServiceSID,
		StartType:        mgr.StartAutomatic,
		SidType:          windows.SERVICE_SID_TYPE_UNRESTRICTED,
		DisplayName:      windowsDisplayName,
	}
	existing, err := m.OpenService(WindowsServiceName)
	switch {
	case err == nil:
		cfg.BinaryPathName = s.cfg.RunnydPath // UpdateConfig has no separate exepath arg, unlike CreateService
		if err := existing.UpdateConfig(cfg); err != nil {
			existing.Close()
			return nil, fmt.Errorf("updating %s: %w", WindowsServiceName, err)
		}
		s.log("service %s already exists — updated binary path", WindowsServiceName)
		return existing, nil
	case errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
		created, err := m.CreateService(WindowsServiceName, s.cfg.RunnydPath, cfg)
		if err != nil {
			return nil, fmt.Errorf("creating service %s: %w", WindowsServiceName, err)
		}
		s.log("created service %s (runs as %s)", WindowsServiceName, windowsServiceSID)
		return created, nil
	default:
		return nil, fmt.Errorf("opening service %s: %w", WindowsServiceName, err)
	}
}

// ensureHome creates the system home (+ logs\) and resets its ACL via icacls.
// Inheritance is disabled first because ProgramData's default DACL grants all
// local Users read, which would leak the GitHub App key.
func (s *scmInstaller) ensureHome(ctx context.Context) error {
	logs := home.Dir(home.SystemHomeDir).LogsDir()
	if err := s.mkdirAll(logs, 0o700); err != nil { // creates home too — logs\ nests under it
		return fmt.Errorf("creating %s: %w", home.SystemHomeDir, err)
	}
	for _, args := range icaclsHomeArgs(home.SystemHomeDir, s.cfg.Operator) {
		if _, err := s.run(ctx, args[0], args[1:]...); err != nil {
			return err
		}
	}
	s.log("prepared %s (owned by %s, ACL grants %s + %s Modify)",
		home.SystemHomeDir, windowsServiceSID, s.cfg.Operator, windowsServiceSID)
	return nil
}

// Uninstall removes the runnyd service AND the home. Unlike darwin, nothing is
// preserved for reinstall: the virtual-account SID derives deterministically
// from WindowsServiceName, so there is no uid-stability concern the way there
// is for darwin's kept _runny account — a reinstall's icacls /setowner
// resolves to the identical SID either way. "Service doesn't exist" only
// skips the stop/delete steps, matching darwin's tolerant bootout: the home is
// still removed either way, so a repeated or partial uninstall is a no-op.
func (s *scmInstaller) Uninstall(ctx context.Context) error {
	m, err := s.connect()
	if err != nil {
		return fmt.Errorf("connecting to the service control manager: %w", err)
	}
	defer m.Disconnect()

	h, err := m.OpenService(WindowsServiceName)
	switch {
	case errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
		s.log("%s is not installed; nothing to stop or delete", WindowsServiceName)
	case err != nil:
		return fmt.Errorf("opening service %s: %w", WindowsServiceName, err)
	default:
		defer h.Close()
		if err := s.stopAndDelete(ctx, h); err != nil {
			return err
		}
	}

	if err := s.removeAll(home.SystemHomeDir); err != nil {
		return fmt.Errorf("removing %s: %w", home.SystemHomeDir, err)
	}
	s.log("removed %s and %s", WindowsServiceName, home.SystemHomeDir)
	return nil
}

func (s *scmInstaller) stopAndDelete(ctx context.Context, h scmService) error {
	if err := s.stopIfRunning(ctx, h); err != nil {
		return err
	}
	if err := h.Delete(); err != nil {
		return fmt.Errorf("deleting %s: %w", WindowsServiceName, err)
	}
	return nil
}

// stopIfRunning stops h if it isn't already, shared by Uninstall's
// stopAndDelete and Install's restart-on-reinstall.
func (s *scmInstaller) stopIfRunning(ctx context.Context, h scmService) error {
	status, err := h.Query()
	if err != nil {
		return fmt.Errorf("querying %s: %w", WindowsServiceName, err)
	}
	if status.State == svc.Stopped {
		return nil
	}
	return s.waitStopped(ctx, h)
}

// waitStopped issues Control(Stop) and polls until h reports Stopped, bounded
// by commandTimeout (30s, shared with darwin's privileged-command ceiling,
// install.go) — no install/uninstall step may block forever. The stop request
// is retried every iteration, not just once: a service in a transitional
// state (StartPending etc.) can reject Control with
// ERROR_SERVICE_CANNOT_ACCEPT_CTRL instead of ERROR_SERVICE_NOT_ACTIVE, and
// Query is authoritative regardless, so retrying converges once the service
// settles rather than failing fast on what is often a momentary rejection.
// Refusing to delete (or restart) a still-running service also avoids a
// marked-for-deletion zombie: a deleted-but-still-running SCM service can't be
// recreated under the same name until the process actually exits.
func (s *scmInstaller) waitStopped(ctx context.Context, h scmService) error {
	cctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	t := time.NewTicker(scmPollInterval)
	defer t.Stop()
	for {
		_, _ = h.Control(svc.Stop)
		status, err := h.Query()
		if err != nil {
			return fmt.Errorf("querying %s: %w", WindowsServiceName, err)
		}
		if status.State == svc.Stopped {
			return nil
		}
		select {
		case <-cctx.Done():
			return fmt.Errorf("%s did not stop within %s; refusing to delete it while running", WindowsServiceName, commandTimeout)
		case <-t.C:
		}
	}
}

// scmPollInterval paces the stop-confirmation poll in waitStopped.
const scmPollInterval = 1 * time.Second
