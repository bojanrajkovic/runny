package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"

	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// The version-skew taxonomy, the Go mirror of the app's SkewVerdictTests. The
// load-bearing correction is normalization: the daemon publishes a suffixed
// label (0.6.0-beta.<sha>) while a release CLI shares the same core, so a same-
// commit pair must compare equal and stay quiet. Pure, so every branch is pinned
// without a live daemon.
func TestVersionSkew(t *testing.T) {
	cases := []struct {
		name                       string
		cliVer, daemonVer          string
		cliProto, daemonProto      uint32
		wantSkew                   bool
		wantContains, wantExcludes string
	}{
		// The common case: at a beta release the SAME commit ships a CLI stamped
		// 0.6.0 and a daemon reporting 0.6.0-beta.<sha>. Raw-string compare would
		// false-alarm on every CI/dev install; normalized cores match → quiet.
		{"beta pair same commit", "0.6.0", "0.6.0-beta.abc12345", 2, 2, false, "", ""},
		// Fresh connect or a daemon predating the version field: nothing heard yet.
		{"empty daemon version", "0.6.0", "", 2, 2, false, "", ""},
		// An unstamped dev build (version "dev", no core) must not wear a false
		// warning against any real daemon.
		{"unstamped dev CLI", "dev", "0.6.0", 2, 2, false, "", ""},
		// The shared-host case: a brew-managed daemon at a wholly different x.y.z.
		{"different release", "0.6.0", "0.5.0", 2, 2, true, "runnyctl is 0.6.0 but runnyd is 0.5.0", ""},
		// The text names the daemon CORE, not its sha-bearing suffix, so a same-
		// core daemon rebuild yields an identical warning (no churn).
		{"mismatch names core not suffix", "0.6.0", "0.5.0-beta.deadbeef", 2, 2, true, "0.5.0", "deadbeef"},
		// The canonical upgrade window: same x.y.z, daemon protocol behind. The
		// version axis is blind (cores match); only the protocol axis sees it.
		{"protocol behind", "0.6.0", "0.6.0", 2, 1, true, "upgrade or restart runnyd", ""},
		// A daemon AHEAD of the CLI degrades nothing — the monotone direction.
		{"newer daemon protocol", "0.6.0", "0.6.0", 2, 3, false, "", ""},
		{"matched", "0.6.0", "0.6.0", 2, 2, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, skewed := versionSkew(tc.cliVer, tc.daemonVer, tc.cliProto, tc.daemonProto)
			if skewed != tc.wantSkew {
				t.Fatalf("versionSkew(%q,%q,%d,%d) skewed=%v, want %v (warning=%q)",
					tc.cliVer, tc.daemonVer, tc.cliProto, tc.daemonProto, skewed, tc.wantSkew, w)
			}
			if !skewed && w != "" {
				t.Errorf("no skew must yield an empty warning, got %q", w)
			}
			if tc.wantContains != "" && !strings.Contains(w, tc.wantContains) {
				t.Errorf("warning %q does not contain %q", w, tc.wantContains)
			}
			if tc.wantExcludes != "" && strings.Contains(w, tc.wantExcludes) {
				t.Errorf("warning %q must exclude the volatile suffix %q", w, tc.wantExcludes)
			}
		})
	}
}

func TestVersionCore(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0.6.0-beta.abc12345", "0.6.0"},
		{"0.6.0", "0.6.0"},
		{"12.34.56-rc.1", "12.34.56"},
		{"", ""},
		{"dev", ""},
		// Anchored at the start (mirroring the build's re.match): a triple that
		// isn't the leading token must not be mis-extracted from the middle.
		{"v0.6.0", ""},
		{"ci-2024.01.15-0.6.0", ""},
	}
	for _, tc := range cases {
		if got := versionCore(tc.in); got != tc.want {
			t.Errorf("versionCore(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// fakeStatusClient stubs GetStatus to drive the warnSkew wiring without a daemon.
type fakeStatusClient struct {
	runnyv1.RunnyServiceClient
	resp *runnyv1.GetStatusResponse
	err  error
}

func (f *fakeStatusClient) GetStatus(_ context.Context, _ *runnyv1.GetStatusRequest, _ ...grpc.CallOption) (*runnyv1.GetStatusResponse, error) {
	return f.resp, f.err
}

func TestWarnSkew(t *testing.T) {
	orig := version
	version = "0.6.0"
	defer func() { version = orig }()

	cases := []struct {
		name         string
		resp         *runnyv1.GetStatusResponse
		err          error
		wantContains string // "" means nothing printed
	}{
		{"version mismatch warns", &runnyv1.GetStatusResponse{Version: "0.5.0", ProtocolVersion: 2}, nil, "different releases"},
		{"protocol behind warns", &runnyv1.GetStatusResponse{Version: "0.6.0", ProtocolVersion: 1}, nil, "predates a capability"},
		{"matched is silent", &runnyv1.GetStatusResponse{Version: "0.6.0", ProtocolVersion: 2}, nil, ""},
		// Best-effort: a down/old daemon fails the probe; the command itself
		// surfaces the real error, so the skew check stays silent here.
		{"status error is silent", nil, errors.New("connection refused"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			c := &ctl{client: &fakeStatusClient{resp: tc.resp, err: tc.err}, err: &buf}
			c.warnSkew(context.Background())
			out := buf.String()
			if tc.wantContains == "" {
				if out != "" {
					t.Errorf("expected no warning, got %q", out)
				}
				return
			}
			if !strings.Contains(out, tc.wantContains) {
				t.Errorf("warning %q does not contain %q", out, tc.wantContains)
			}
		})
	}
}
