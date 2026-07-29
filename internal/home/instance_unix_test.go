//go:build unix

package home

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// withUmask installs a umask for the duration of a test. Process-global, so it
// relies on tests in this package not running in parallel.
func withUmask(t *testing.T, mask int) {
	t.Helper()
	prev := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(prev) })
}

// os.WriteFile's mode is a REQUEST — the umask masks it — so a daemon started
// from a shell with a restrictive umask would write instance-id at 0600 again,
// silently reintroducing exactly what the 0644 exists to prevent: an id the
// service account may be unable to read, which InstancePrefix surfaces as a
// hard error rather than regenerating.
func TestInstancePrefixModeSurvivesARestrictiveUmask(t *testing.T) {
	withUmask(t, 0o077)
	d := Dir(t.TempDir())

	if _, err := d.InstancePrefix(); err != nil {
		t.Fatalf("generating the instance id: %v", err)
	}
	st, err := os.Stat(d.InstanceIDPath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("instance-id under umask 077 = %v, want 0644", st.Mode().Perm())
	}
}

// An id written by an older runny keeps its 0600 forever unless something
// re-asserts it — the same reason Ensure re-Chmods every directory it creates
// and openRotatingFile re-Chmods the log.
func TestEnsureReassertsAnExistingInstanceIDMode(t *testing.T) {
	d := Dir(t.TempDir())
	if err := d.Ensure(); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if err := os.WriteFile(d.InstanceIDPath(), []byte("stale-00112233\n"), 0o600); err != nil {
		t.Fatalf("seeding a legacy instance-id: %v", err)
	}

	if err := d.Ensure(); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	st, err := os.Stat(d.InstanceIDPath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("instance-id after Ensure = %v, want 0644 re-asserted", st.Mode().Perm())
	}
	b, err := os.ReadFile(d.InstanceIDPath())
	if err != nil || string(b) != "stale-00112233\n" {
		t.Errorf("Ensure must re-mode the id, never rewrite it: content = %q, err = %v", b, err)
	}
}

// A home with no id yet is the normal first-start shape: Ensure must not
// create one (that is InstancePrefix's job, gated on the home being the
// daemon's own) and must not fail for its absence.
func TestEnsureToleratesAnAbsentInstanceID(t *testing.T) {
	d := Dir(t.TempDir())
	if err := d.Ensure(); err != nil {
		t.Fatalf("Ensure on a home with no instance-id: %v", err)
	}
	if _, err := os.Stat(filepath.Join(string(d), "instance-id")); !os.IsNotExist(err) {
		t.Errorf("Ensure created an instance-id (stat err = %v), want none", err)
	}
}
