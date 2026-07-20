//go:build windows

package sysdaemon

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/bojanrajkovic/runny/internal/home"
)

// fakeService is a scmService double. All calls append to a log shared with
// fakeMgr and orderedRun, so tests can assert cross-seam ordering (service
// before icacls) without threading two separate call histories together.
type fakeService struct {
	name        string
	status      svc.Status
	calls       *[]string
	lastConfig  mgr.Config
	neverStops  bool // simulates a service the SCM never actually stops
	settleAfter int  // Control rejects with ERROR_SERVICE_CANNOT_ACCEPT_CTRL this many times before stopping
}

func (f *fakeService) UpdateConfig(c mgr.Config) error {
	*f.calls = append(*f.calls, "update:"+f.name)
	f.lastConfig = c
	return nil
}

func (f *fakeService) SetRecoveryActions(actions []mgr.RecoveryAction, resetPeriod uint32) error {
	*f.calls = append(*f.calls, "recovery")
	return nil
}

func (f *fakeService) SetRecoveryActionsOnNonCrashFailures(flag bool) error {
	*f.calls = append(*f.calls, fmt.Sprintf("nonCrashRecovery:%v", flag))
	return nil
}

func (f *fakeService) Start(args ...string) error {
	*f.calls = append(*f.calls, "start")
	return nil
}

func (f *fakeService) Control(c svc.Cmd) (svc.Status, error) {
	*f.calls = append(*f.calls, "stop")
	if f.neverStops {
		return f.status, nil
	}
	if f.settleAfter > 0 {
		f.settleAfter--
		return f.status, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL
	}
	if f.status.State == svc.Stopped {
		return f.status, windows.ERROR_SERVICE_NOT_ACTIVE
	}
	f.status.State = svc.Stopped
	return f.status, nil
}

func (f *fakeService) Query() (svc.Status, error) { return f.status, nil }

func (f *fakeService) Delete() error {
	*f.calls = append(*f.calls, "delete")
	return nil
}

func (f *fakeService) Close() error { return nil }

// fakeMgr is a scmMgr double: existing non-nil makes OpenService succeed (the
// idempotent-reinstall / uninstall-existing path); nil makes it report
// ERROR_SERVICE_DOES_NOT_EXIST (the fresh-install / already-uninstalled path).
type fakeMgr struct {
	existing scmService
	calls    *[]string
}

func (f *fakeMgr) OpenService(name string) (scmService, error) {
	if f.existing == nil {
		return nil, windows.ERROR_SERVICE_DOES_NOT_EXIST
	}
	return f.existing, nil
}

func (f *fakeMgr) CreateService(name, exepath string, c mgr.Config) (scmService, error) {
	*f.calls = append(*f.calls, "create:"+name)
	// Stopped, not Running: the real SCM's CreateService does not start the
	// service — Install's own Start() call does that.
	s := &fakeService{name: name, calls: f.calls, lastConfig: c, status: svc.Status{State: svc.Stopped}}
	f.existing = s
	return s, nil
}

func (f *fakeMgr) Disconnect() error { return nil }

// orderedRun is a fake Runner that records raw icacls calls (for exactCall
// assertions) AND appends into the same shared order log the SCM fakes use,
// so a test can assert ordering across both seams.
type orderedRun struct {
	calls [][]string
	order *[]string
}

func (r *orderedRun) run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	*r.order = append(*r.order, "run:"+name)
	return "", nil
}

func newTestScmInstaller(m *fakeMgr, r *orderedRun) *scmInstaller {
	return &scmInstaller{
		cfg:       Config{Operator: testOperator, RunnydPath: `C:\runny\runnyd.exe`},
		connect:   func() (scmMgr, error) { return m, nil },
		run:       r.run,
		writeFile: func(string, []byte, os.FileMode) error { return nil },
		mkdirAll:  func(string, os.FileMode) error { return nil },
		removeAll: func(string) error { return nil },
		log:       func(string, ...any) {},
	}
}

func TestScmInstallOrdersServiceBeforeHomeACL(t *testing.T) {
	var order []string
	m := &fakeMgr{calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	createIdx := slices.Index(order, "create:"+WindowsServiceName)
	firstIcaclsIdx := slices.Index(order, "run:icacls")
	if createIdx < 0 {
		t.Fatal("service was never created")
	}
	if firstIcaclsIdx < 0 {
		t.Fatal("no icacls call was made")
	}
	if createIdx > firstIcaclsIdx {
		t.Errorf("service must be created (idx %d) BEFORE any icacls call (idx %d) — the SID does not resolve until then",
			createIdx, firstIcaclsIdx)
	}
}

func TestScmInstallConfiguresRecovery(t *testing.T) {
	var order []string
	m := &fakeMgr{calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if slices.Index(order, "recovery") < 0 {
		t.Error("SetRecoveryActions was never called")
	}
	if slices.Index(order, "nonCrashRecovery:true") < 0 {
		t.Error("SetRecoveryActionsOnNonCrashFailures(true) was never called")
	}
}

func TestScmInstallRunsIcaclsSequence(t *testing.T) {
	var order []string
	m := &fakeMgr{calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, want := range icaclsHomeArgs(home.SystemHomeDir, testOperator) {
		if !exactCall(r.calls, want...) {
			t.Errorf("missing icacls step: %v", want)
		}
	}
}

func TestScmInstallReinstallUpdatesExistingService(t *testing.T) {
	var order []string
	existing := &fakeService{name: WindowsServiceName, calls: &order, status: svc.Status{State: svc.Running}}
	m := &fakeMgr{existing: existing, calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if slices.Index(order, "create:"+WindowsServiceName) >= 0 {
		t.Error("an existing service must not be recreated")
	}
	if slices.Index(order, "update:"+WindowsServiceName) < 0 {
		t.Error("an existing service's config must be updated (idempotent reinstall)")
	}
}

// Regression test: Install used to call Start() unconditionally, which the
// real SCM rejects with ERROR_SERVICE_ALREADY_RUNNING for a service the
// reinstall found already running — silently failing the documented
// idempotent-reinstall path (and never applying the updated BinaryPathName,
// since that only takes effect on the next start). Install must stop a
// running service before starting it again.
func TestScmInstallReinstallOverRunningServiceStopsBeforeStarting(t *testing.T) {
	var order []string
	existing := &fakeService{name: WindowsServiceName, calls: &order, status: svc.Status{State: svc.Running}}
	m := &fakeMgr{existing: existing, calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	stopIdx := slices.Index(order, "stop")
	startIdx := slices.Index(order, "start")
	if stopIdx < 0 {
		t.Fatal("a running service was never stopped before restart")
	}
	if startIdx < 0 {
		t.Fatal("service was never started")
	}
	if stopIdx > startIdx {
		t.Errorf("service must be stopped (idx %d) BEFORE it is started again (idx %d)", stopIdx, startIdx)
	}
}

func TestScmUninstallStopsAndDeletes(t *testing.T) {
	var order []string
	existing := &fakeService{name: WindowsServiceName, calls: &order, status: svc.Status{State: svc.Running}}
	m := &fakeMgr{existing: existing, calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	var removedPath string
	inst.removeAll = func(path string) error { removedPath = path; return nil }
	if err := inst.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if slices.Index(order, "stop") < 0 {
		t.Error("a running service must be stopped")
	}
	if slices.Index(order, "delete") < 0 {
		t.Error("the service must be deleted after it stops")
	}
	if removedPath != home.SystemHomeDir {
		t.Errorf("removeAll called with %q, want %q", removedPath, home.SystemHomeDir)
	}
}

func TestScmUninstallRefusesStuckService(t *testing.T) {
	var order []string
	stuck := &fakeService{name: WindowsServiceName, calls: &order, status: svc.Status{State: svc.Running}, neverStops: true}
	m := &fakeMgr{existing: stuck, calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	// waitStopped bounds its poll at commandTimeout via context.WithTimeout —
	// a parent context with a short deadline wins the min(), so this test
	// doesn't wait out the real 30s ceiling.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := inst.Uninstall(ctx); err == nil {
		t.Fatal("Uninstall must refuse a service that never reports Stopped")
	}
	if slices.Index(order, "delete") >= 0 {
		t.Error("must not delete a service that never confirmed it stopped")
	}
}

// A service in a transitional state (StartPending etc.) can reject
// Control(Stop) with ERROR_SERVICE_CANNOT_ACCEPT_CTRL instead of stopping —
// waitStopped must retry rather than treat that as fatal.
func TestScmUninstallRetriesThroughTransitionalState(t *testing.T) {
	var order []string
	existing := &fakeService{name: WindowsServiceName, calls: &order, status: svc.Status{State: svc.Running}, settleAfter: 1}
	m := &fakeMgr{existing: existing, calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	if err := inst.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall must converge once the transitional state settles: %v", err)
	}
	if slices.Index(order, "delete") < 0 {
		t.Error("service must be deleted once it stops")
	}
}

func TestScmUninstallToleratesMissingService(t *testing.T) {
	var order []string
	m := &fakeMgr{existing: nil, calls: &order}
	r := &orderedRun{order: &order}
	inst := newTestScmInstaller(m, r)
	if err := inst.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall of a never-installed service must be a no-op success: %v", err)
	}
}
