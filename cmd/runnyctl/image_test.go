package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestImagePackCommandStringMatchesMainSwitch is a regression test for
// main.go's early-return switch, which matches the literal string
// "image pack <disk>" to route this command around the daemon-dial setup
// (a code-review found that string is coupled to kong's positional-arg
// rendering and to ImagePackCmd.Disk's own `name:"disk"` tag -- a future
// rename or kong upgrade could silently break the match with nothing to
// catch it). This asserts the exact string main.go's switch depends on.
func TestImagePackCommandStringMatchesMainSwitch(t *testing.T) {
	cli := &CLI{}
	parser, err := newKong(cli, io.Discard, io.Discard, func(int) {})
	if err != nil {
		t.Fatalf("newKong: %v", err)
	}
	kctx, err := parser.Parse([]string{"image", "pack", "disk.img", "--os", "windows", "--arch", "amd64", "--cpu-count", "1", "--memory-size", "1073741824"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := kctx.Command(); got != "image pack <disk>" {
		t.Errorf("kctx.Command() = %q, want %q -- main.go's early-return switch needs updating to match", got, "image pack <disk>")
	}
}

// TestImagePackCmdRun exercises ImagePackCmd.Run -> imagePack end to end
// through the real kong grammar, guarding the argument order between the
// struct's fields and imagePack's positional parameters (nothing else would
// catch a transposition there, e.g. os/arch swapped, since both are
// same-typed strings): WriteImage's config-validation self-check rejects an
// unsupported OS/Arch combination before a single blob is written, so a
// transposition here would fail this test rather than pack silently.
func TestImagePackCmdRun(t *testing.T) {
	dir := t.TempDir()
	diskPath := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(diskPath, []byte("fake disk bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	layoutDir := filepath.Join(dir, "out")

	c := &ctl{out: io.Discard, err: io.Discard}
	err := runArgs(t, c,
		"image", "pack", diskPath,
		"--oci-layout", layoutDir,
		"--os", "windows", "--arch", "amd64",
		"--cpu-count", "2", "--memory-size", "4294967296",
	)
	if err != nil {
		t.Fatalf("image pack: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(layoutDir, "blobs", "sha256"))
	if err != nil {
		t.Fatalf("reading packed layout: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("image pack produced no blobs")
	}
}

// TestImagePackRequiresNVRAMForNonWindows guards the fix for a code-review
// finding: the nvram placeholder is only safe for windows guests (HCS never
// reads NVRAMPath; VZ, which darwin/linux boot through, parses it as real
// EFI/aux-storage firmware state), so imagePack must refuse to silently
// reuse it for the other two guest OSes. Only "linux" goes through the CLI
// (a later fix, also from Codex review on this PR, makes kong itself reject
// --os darwin via an enum constraint -- see ImagePackCmd.OS -- since this
// command has no way to supply the hardwareModel/ecid a darwin config
// needs); "darwin" is exercised by calling imagePack directly instead, so
// this function's own defense stays covered independent of what the CLI
// currently exposes.
func TestImagePackRequiresNVRAMForNonWindows(t *testing.T) {
	dir := t.TempDir()
	diskPath := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(diskPath, []byte("fake disk bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("linux via CLI", func(t *testing.T) {
		c := &ctl{out: io.Discard, err: io.Discard}
		err := runArgs(t, c,
			"image", "pack", diskPath,
			"--oci-layout", filepath.Join(dir, "out-linux"),
			"--os", "linux", "--arch", "amd64",
			"--cpu-count", "2", "--memory-size", "4294967296",
		)
		if err == nil {
			t.Fatal("want an error requiring --nvram for --os linux, got nil")
		}
	})

	t.Run("darwin via imagePack directly", func(t *testing.T) {
		err := imagePack(diskPath, filepath.Join(dir, "out-darwin"), "darwin", "arm64", 2, 4294967296, "")
		if err == nil {
			t.Fatal("want an error requiring --nvram for darwin, got nil")
		}
	})
}
