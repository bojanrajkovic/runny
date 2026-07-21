//go:build windows

package vhdx

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// fakeBackend fakes go-winio/vhd's calls so convert's control flow (success
// and every cleanup path) is testable without a live elevated session or
// real disk allocation — same shape as internal/sysdaemon's scmMgr fakes.
type fakeBackend struct {
	calls []string

	createErr   error
	attachErr   error
	physPathErr error
	detachErr   error
	closeErr    error

	physPath string // the file convert's copyPayload writes into, standing in for \\.\PhysicalDriveN
}

func (f *fakeBackend) createFixed(path string, maximumSize uint64) (syscall.Handle, error) {
	f.calls = append(f.calls, "createFixed")
	if f.createErr != nil {
		// The real CreateVirtualDisk leaves its handle out-param at zero
		// on failure; match that so a caller that (mis)uses the handle on
		// an error path fails the same way here as against the real API.
		return 0, f.createErr
	}
	return 1, nil
}

func (f *fakeBackend) attach(handle syscall.Handle) error {
	f.calls = append(f.calls, "attach")
	return f.attachErr
}

func (f *fakeBackend) physicalPath(handle syscall.Handle) (string, error) {
	f.calls = append(f.calls, "physicalPath")
	return f.physPath, f.physPathErr
}

func (f *fakeBackend) detach(handle syscall.Handle) error {
	f.calls = append(f.calls, "detach")
	return f.detachErr
}

func (f *fakeBackend) closeHandle(handle syscall.Handle) error {
	f.calls = append(f.calls, "closeHandle")
	return f.closeErr
}

func writeTempFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	return path
}

// validSourceContent is minSourceSize bytes (the CreateVirtualDisk floor),
// a multiple of the logical sector size — passes both of convert's
// pre-flight checks, so tests using it reach the backend.
func validSourceContent(t *testing.T) []byte {
	t.Helper()
	buf := make([]byte, minSourceSize)
	for i := range buf {
		buf[i] = byte(i)
	}
	return buf
}

func TestConvert_Success(t *testing.T) {
	dir := t.TempDir()
	content := validSourceContent(t)
	src := writeTempFile(t, dir, "src.img", content)
	dst := filepath.Join(dir, "dst.vhdx")
	device := filepath.Join(dir, "fake-device") // stands in for \\.\PhysicalDriveN
	if err := os.WriteFile(device, nil, 0o600); err != nil {
		t.Fatalf("seeding fake device: %v", err)
	}

	b := &fakeBackend{physPath: device}
	if err := convert(src, dst, b); err != nil {
		t.Fatalf("convert: %v", err)
	}

	wantCalls := []string{"createFixed", "attach", "physicalPath", "detach", "closeHandle"}
	if !slices.Equal(b.calls, wantCalls) {
		t.Errorf("calls = %v, want %v", b.calls, wantCalls)
	}

	got, err := os.ReadFile(device)
	if err != nil {
		t.Fatalf("reading fake device: %v", err)
	}
	if !slices.Equal(got, content) {
		t.Error("device content does not match source content")
	}
}

func TestConvert_CreateFails_RemovesDst(t *testing.T) {
	dir := t.TempDir()
	src := writeTempFile(t, dir, "src.img", validSourceContent(t))
	dst := writeTempFile(t, dir, "dst.vhdx", []byte("partial")) // stand-in for whatever CreateVirtualDisk left behind

	b := &fakeBackend{createErr: errors.New("boom")}
	err := convert(src, dst, b)
	if err == nil {
		t.Fatal("convert: want error, got nil")
	}
	wantCalls := []string{"createFixed"}
	if !slices.Equal(b.calls, wantCalls) {
		t.Errorf("calls = %v, want %v (no attach/detach after a failed create)", b.calls, wantCalls)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("dst %s still exists after a failed create, want removed", dst)
	}
}

func TestConvert_AttachFails_CleansUp(t *testing.T) {
	dir := t.TempDir()
	src := writeTempFile(t, dir, "src.img", validSourceContent(t))
	dst := writeTempFile(t, dir, "dst.vhdx", []byte("partial")) // stand-in for what createFixed would have produced

	b := &fakeBackend{attachErr: errors.New("boom")}
	err := convert(src, dst, b)
	if err == nil {
		t.Fatal("convert: want error, got nil")
	}
	wantCalls := []string{"createFixed", "attach", "closeHandle"}
	if !slices.Equal(b.calls, wantCalls) {
		t.Errorf("calls = %v, want %v (no detach: never successfully attached)", b.calls, wantCalls)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("dst %s still exists after a failed attach, want removed", dst)
	}
}

func TestConvert_PhysicalPathFails_CleansUp(t *testing.T) {
	dir := t.TempDir()
	src := writeTempFile(t, dir, "src.img", validSourceContent(t))
	dst := writeTempFile(t, dir, "dst.vhdx", []byte("partial"))

	b := &fakeBackend{physPathErr: errors.New("boom")}
	err := convert(src, dst, b)
	if err == nil {
		t.Fatal("convert: want error, got nil")
	}
	wantCalls := []string{"createFixed", "attach", "physicalPath", "detach", "closeHandle"}
	if !slices.Equal(b.calls, wantCalls) {
		t.Errorf("calls = %v, want %v (attach succeeded, so detach still runs)", b.calls, wantCalls)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("dst %s still exists after a failed physicalPath, want removed", dst)
	}
}

func TestConvert_CopyFails_DetachesAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	src := writeTempFile(t, dir, "src.img", validSourceContent(t))
	dst := writeTempFile(t, dir, "dst.vhdx", []byte("partial"))

	b := &fakeBackend{physPath: filepath.Join(dir, "no-such-directory", "fake-device")} // copyPayload's OpenFile fails: parent dir doesn't exist
	err := convert(src, dst, b)
	if err == nil {
		t.Fatal("convert: want error, got nil")
	}
	wantCalls := []string{"createFixed", "attach", "physicalPath", "detach", "closeHandle"}
	if !slices.Equal(b.calls, wantCalls) {
		t.Errorf("calls = %v, want %v (attach succeeded, so detach still runs)", b.calls, wantCalls)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("dst %s still exists after a failed copy, want removed", dst)
	}
}

// TestConvert_DetachFails_PreservesCompletedFile is the regression test for
// the bug where a fully-successful conversion got deleted just because the
// trailing detach failed — the payload is valid at that point, so dst must
// survive even though convert still reports the detach error.
func TestConvert_DetachFails_PreservesCompletedFile(t *testing.T) {
	dir := t.TempDir()
	content := validSourceContent(t)
	src := writeTempFile(t, dir, "src.img", content)
	dst := writeTempFile(t, dir, "dst.vhdx", []byte("this is the completed VHDX"))
	device := writeTempFile(t, dir, "fake-device", nil)

	b := &fakeBackend{physPath: device, detachErr: errors.New("boom")}
	err := convert(src, dst, b)
	if err == nil {
		t.Fatal("convert: want the detach error surfaced, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("convert error = %q, want it to mention the detach failure", err)
	}
	if _, statErr := os.Stat(dst); statErr != nil {
		t.Errorf("dst %s was removed after a completed copy + failed detach, want preserved: %v", dst, statErr)
	}
}

// TestConvert_DetachAndCloseBothFail_ErrorsJoined is the regression test
// for the bug where a second cleanup error (closeHandle) was silently
// dropped whenever detach had already failed.
func TestConvert_DetachAndCloseBothFail_ErrorsJoined(t *testing.T) {
	dir := t.TempDir()
	src := writeTempFile(t, dir, "src.img", validSourceContent(t))
	dst := writeTempFile(t, dir, "dst.vhdx", nil)
	device := writeTempFile(t, dir, "fake-device", nil)

	b := &fakeBackend{physPath: device, detachErr: errors.New("detach boom"), closeErr: errors.New("close boom")}
	err := convert(src, dst, b)
	if err == nil {
		t.Fatal("convert: want error, got nil")
	}
	if !strings.Contains(err.Error(), "detach boom") || !strings.Contains(err.Error(), "close boom") {
		t.Errorf("convert error = %q, want both the detach and close failures present", err)
	}
}

func TestConvert_EmptySource(t *testing.T) {
	dir := t.TempDir()
	src := writeTempFile(t, dir, "src.img", nil)
	dst := filepath.Join(dir, "dst.vhdx")

	b := &fakeBackend{}
	if err := convert(src, dst, b); err == nil {
		t.Fatal("convert(empty source): want error, got nil")
	}
	if len(b.calls) != 0 {
		t.Errorf("calls = %v, want none (rejected before touching the backend)", b.calls)
	}
}

func TestConvert_TooSmallSource(t *testing.T) {
	dir := t.TempDir()
	src := writeTempFile(t, dir, "src.img", make([]byte, 1024*1024)) // 1 MiB: nonzero, still below the 3 MiB floor
	dst := filepath.Join(dir, "dst.vhdx")

	b := &fakeBackend{}
	err := convert(src, dst, b)
	if err == nil {
		t.Fatal("convert(1 MiB source): want error, got nil")
	}
	if !strings.Contains(err.Error(), "minimum") {
		t.Errorf("convert error = %q, want it to name the size floor", err)
	}
	if len(b.calls) != 0 {
		t.Errorf("calls = %v, want none (rejected before touching the backend)", b.calls)
	}
}

func TestConvert_UnalignedSource(t *testing.T) {
	dir := t.TempDir()
	src := writeTempFile(t, dir, "src.img", make([]byte, minSourceSize+1)) // one byte past a sector boundary
	dst := filepath.Join(dir, "dst.vhdx")

	b := &fakeBackend{}
	err := convert(src, dst, b)
	if err == nil {
		t.Fatal("convert(unaligned source): want error, got nil")
	}
	if !strings.Contains(err.Error(), "sector size") {
		t.Errorf("convert error = %q, want it to mention sector alignment", err)
	}
	if len(b.calls) != 0 {
		t.Errorf("calls = %v, want none (rejected before touching the backend)", b.calls)
	}
}
