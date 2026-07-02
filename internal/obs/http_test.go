package obs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// scopedHTTP returns a context carrying a cycle scope whose events land in
// the returned slice, stepped into ENSURE_IMAGE.
func scopedHTTP(events *[]Event) context.Context {
	ctx := WithCycle(context.Background(), func(e Event) { *events = append(*events, e) }, testCycle())
	return WithStep(ctx, "ENSURE_IMAGE")
}

// One scoped round trip emits exactly one KindHTTP event carrying the
// annotated class, the method, the request host, the response status, and a
// positive duration — stamped with the scope's step like any other event.
func TestHTTPTransportEmitsOneEventPerRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	var events []Event
	ctx := WithHTTPClass(scopedHTTP(&events), HTTPGitHubJIT)

	hc := &http.Client{Transport: &HTTPTransport{}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/mint?secret=nope", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.Kind != KindHTTP || e.HTTP == nil {
		t.Fatalf("got kind %q (payload %v), want KindHTTP with payload", e.Kind, e.HTTP)
	}
	if e.Step != "ENSURE_IMAGE" {
		t.Errorf("Step = %q, want ENSURE_IMAGE", e.Step)
	}
	h := e.HTTP
	if h.Class != HTTPGitHubJIT {
		t.Errorf("Class = %q, want %q", h.Class, HTTPGitHubJIT)
	}
	if h.Method != http.MethodPost {
		t.Errorf("Method = %q, want POST", h.Method)
	}
	if h.Status != http.StatusCreated {
		t.Errorf("Status = %d, want 201", h.Status)
	}
	if h.Host == "" || strings.ContainsAny(h.Host, "/?") {
		t.Errorf("Host = %q, want bare hostname with no path or query", h.Host)
	}
	if h.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", h.Duration)
	}
	if h.Error != "" {
		t.Errorf("Error = %q, want empty", h.Error)
	}
}

// A transport-level failure still emits: status 0, the error text — and the
// error must not leak the URL (path and query are never recorded).
func TestHTTPTransportEmitsOnTransportError(t *testing.T) {
	var events []Event
	ctx := WithHTTPClass(scopedHTTP(&events), HTTPRegistryManifest)

	boom := errors.New("dial refused")
	hc := &http.Client{Transport: &HTTPTransport{Base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, boom
	})}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://registry.example/v2/org/repo/manifests/latest?tok=x", nil)
	if _, err := hc.Do(req); err == nil {
		t.Fatal("want error from failed round trip")
	}

	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	h := events[0].HTTP
	if h.Status != 0 {
		t.Errorf("Status = %d, want 0", h.Status)
	}
	if h.Error == "" {
		t.Error("Error empty, want transport error text")
	}
	if strings.Contains(h.Error, "manifests") || strings.Contains(h.Error, "tok=") {
		t.Errorf("Error %q leaks URL path or query", h.Error)
	}
}

// A scoped request with no class annotation is still visible — classed
// HTTPOther, never dropped and never a raw URL.
func TestHTTPTransportUnannotatedClassIsOther(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	var events []Event
	hc := &http.Client{Transport: &HTTPTransport{}}
	req, _ := http.NewRequestWithContext(scopedHTTP(&events), http.MethodGet, srv.URL, nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(events) != 1 || events[0].HTTP.Class != HTTPOther {
		t.Fatalf("events = %+v, want one event classed %q", events, HTTPOther)
	}
}

// A scope-less request — the shared pull actor's blob traffic, startup
// doctor checks — passes straight through: zero events, zero extra work.
func TestHTTPTransportScopelessIsPassthrough(t *testing.T) {
	called := false
	hc := &http.Client{Transport: &HTTPTransport{Base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://registry.example/v2/blob", nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !called {
		t.Fatal("base transport not called")
	}
	// No scope → nothing to emit into; nothing to assert beyond not panicking
	// and delegating. The scoped tests prove events flow when a scope exists.
}

// The event's Time is stamped at completion; Duration reaches back to the
// start of the round trip, so a consumer reconstructs the start as
// Time − Duration without a second event to pair with.
func TestHTTPTransportDurationCoversRoundTrip(t *testing.T) {
	const wait = 30 * time.Millisecond
	var events []Event
	hc := &http.Client{Transport: &HTTPTransport{Base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		time.Sleep(wait)
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}}
	req, _ := http.NewRequestWithContext(WithHTTPClass(scopedHTTP(&events), HTTPTarballDownload), http.MethodGet, "https://cdn.example/x", nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if d := events[0].HTTP.Duration; d < wait {
		t.Errorf("Duration = %v, want ≥ %v", d, wait)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
