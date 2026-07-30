package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// configClient stubs GetConfig alone; every other RPC panics through the
// embedded nil interface, which is what keeps a test honest about which calls
// the code under test actually makes.
type configClient struct {
	runnyv1.RunnyServiceClient
	resp *runnyv1.GetConfigResponse
	err  error
}

func (c *configClient) GetConfig(ctx context.Context, in *runnyv1.GetConfigRequest, opts ...grpc.CallOption) (*runnyv1.GetConfigResponse, error) {
	return c.resp, c.err
}

func TestConfigBytesPrefersTheDaemon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("stale: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &ctl{client: &configClient{resp: &runnyv1.GetConfigResponse{Content: []byte("live: true\n")}}}

	got, err := c.configBytes(t.Context(), path)
	if err != nil {
		t.Fatalf("configBytes: %v", err)
	}
	if string(got) != "live: true\n" {
		t.Errorf("got %q, want the daemon's bytes — a readable file on disk must not win over the daemon", got)
	}
}

// The daemon saying "there is no config" is authoritative, and is what lets
// edit-config seed a fresh skeleton.
func TestConfigBytesMapsNotFoundToErrNotExist(t *testing.T) {
	c := &ctl{client: &configClient{err: status.Error(codes.NotFound, "no config")}}
	_, err := c.configBytes(t.Context(), filepath.Join(t.TempDir(), "config.yaml"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist in the chain", err)
	}
}

// A daemon that is down (or predates GetConfig) falls back to a direct read —
// the normal path for a per-user home, whose owner is the operator.
func TestConfigBytesFallsBackToDiskWhenTheDaemonIsUnreachable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("on: disk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, code := range []codes.Code{codes.Unavailable, codes.Unimplemented} {
		c := &ctl{client: &configClient{err: status.Error(code, "nope")}}
		got, err := c.configBytes(t.Context(), path)
		if err != nil {
			t.Fatalf("%s: configBytes: %v", code, err)
		}
		if string(got) != "on: disk\n" {
			t.Errorf("%s: got %q, want the on-disk bytes", code, got)
		}
	}
}

func TestConfigBytesReportsAnAbsentFileAsErrNotExist(t *testing.T) {
	c := &ctl{client: &configClient{err: status.Error(codes.Unavailable, "nope")}}
	_, err := c.configBytes(t.Context(), filepath.Join(t.TempDir(), "config.yaml"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist in the chain", err)
	}
}

// The one that matters. With the daemon unreachable AND the file unreadable —
// a stopped system daemon, seen by an operator whose ACL entry stops at the
// home directory — the answer must NOT look like "no config exists". Callers
// seed a blank skeleton on ErrNotExist, and applying that over a live fleet's
// config destroys it. Skipped as root, which reads through mode bits.
func TestConfigBytesDoesNotDisguiseAnUnreadableConfigAsAbsent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not gate reads on windows -- os.WriteFile with 0o000 yields a perfectly readable file, so the present-but-unreadable state this pins cannot be constructed there")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads through mode bits; the distinction is unobservable")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("pools: [{name: prod}]\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	c := &ctl{client: &configClient{err: status.Error(codes.Unavailable, "connection refused")}}

	_, err := c.configBytes(t.Context(), path)
	if err == nil {
		t.Fatal("expected an error for an unreadable config")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreadable config reported as absent (%v); the caller would seed a blank skeleton over it", err)
	}
	// Both attempts failed for different reasons; naming only one of them sends
	// the operator to the wrong fix.
	for _, want := range []string{"connection refused", path} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// setConfigClient stubs the write side. Unavailable is what a stopped daemon
// looks like, which is the branch that writes to disk.
type setConfigClient struct {
	runnyv1.RunnyServiceClient
	err error
}

func (c *setConfigClient) SetConfig(ctx context.Context, in *runnyv1.SetConfigRequest, opts ...grpc.CallOption) (*runnyv1.SetConfigResponse, error) {
	return &runnyv1.SetConfigResponse{}, c.err
}

// A per-user host where the daemon has never run has no ~/.runny at all, and
// edit-config is what creates it. Without this the fallback write fails on a
// directory that does not exist -- after the operator has already done the edit.
func TestApplyConfigCreatesTheHomeWhenTheDaemonIsDown(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "never-ran", "config.yaml")
	c := &ctl{client: &setConfigClient{err: status.Error(codes.Unavailable, "connection refused")}}

	if err := c.applyConfig(t.Context(), configPath, []byte("pools: []\n")); err != nil {
		t.Fatalf("applyConfig: %v", err)
	}
	on, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("config was not written: %v", err)
	}
	if string(on) != "pools: []\n" {
		t.Errorf("config = %q, want the bytes passed in", on)
	}
}

// A daemon that answers and REFUSES must not be written around -- that would
// apply an edit it rejected.
func TestApplyConfigDoesNotWriteBehindARefusingDaemon(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	c := &ctl{client: &setConfigClient{err: status.Error(codes.PermissionDenied, "revoked")}}

	if err := c.applyConfig(t.Context(), configPath, []byte("pools: []\n")); err == nil {
		t.Fatal("expected the refusal to propagate")
	}
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config was written despite the daemon refusing (stat err %v)", err)
	}
}

// The precedence chain, the whitespace-only cases, and that the fallback path
// is reached at all — none of which the per-platform defaultEditor tests cover.
//
// What it deliberately does NOT prove, on a unix host: that the fallback goes
// through defaultEditor rather than a hardcoded "vi". Those two agree here, so
// the assertion cannot tell them apart. It is the windows-2022 lane running
// this same table that catches a regression to a unix literal — the platform
// whose default differs is the only one where the distinction is observable.
func TestEditorArgvPrefersVisualThenEditorThenPlatformDefault(t *testing.T) {
	for _, tc := range []struct {
		name           string
		visual, editor string
		want           []string
	}{
		{"visual wins over editor", "code --wait", "vim", []string{"code", "--wait"}},
		{"editor when visual is unset", "", "nano", []string{"nano"}},
		{"whitespace is not a choice", "   ", "\t", []string{defaultEditor()}},
		{"platform default when neither is set", "", "", []string{defaultEditor()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VISUAL", tc.visual)
			t.Setenv("EDITOR", tc.editor)
			got := editorArgv()
			if !slices.Equal(got, tc.want) {
				t.Errorf("editorArgv() = %q, want %q", got, tc.want)
			}
		})
	}
}
