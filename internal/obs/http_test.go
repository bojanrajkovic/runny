package obs

import (
	"context"
	"errors"
	"io"
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The event emits at body completion, not headers: Duration covers the full
// transfer, BytesRead counts the body, and HeaderDuration marks when
// headers arrived — a slow multi-hundred-MB download is a long span, not a
// millisecond one.
func TestHTTPTransportDurationCoversBodyTransfer(t *testing.T) {
	const wait = 30 * time.Millisecond
	body := []byte("0123456789")

	var events []Event
	hc := &http.Client{Transport: &HTTPTransport{Base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		// A real (if tiny) delay before the mock returns headers: with no
		// delay at all, HeaderDuration measures two time.Now() calls with
		// nothing but a struct literal between them, and on a CI host with
		// coarser clock granularity that can legitimately read back as
		// exactly 0 — not a bug in HeaderDuration, just an unmeasurable gap.
		time.Sleep(time.Millisecond)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(&slowReader{data: body, delay: wait})}, nil
	})}}
	req, _ := http.NewRequestWithContext(WithHTTPClass(scopedHTTP(&events), HTTPTarballDownload), http.MethodGet, "https://cdn.example/x", nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("event emitted at headers; want none until the body completes (got %+v)", events)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(events) != 1 {
		t.Fatalf("got %d events after body close, want 1", len(events))
	}
	h := events[0].HTTP
	if h.Duration < wait {
		t.Errorf("Duration = %v, want ≥ %v (the body transfer)", h.Duration, wait)
	}
	if h.HeaderDuration <= 0 || h.HeaderDuration > h.Duration {
		t.Errorf("HeaderDuration = %v, want in (0, %v]", h.HeaderDuration, h.Duration)
	}
	if h.BytesRead != int64(len(body)) {
		t.Errorf("BytesRead = %d, want %d", h.BytesRead, len(body))
	}
	if h.Status != http.StatusOK || h.Error != "" {
		t.Errorf("event = %+v, want clean 200", h)
	}
}

// A body that dies mid-transfer (the stall-kill shape) still emits exactly
// once, with the bytes that made it, the 200 the headers claimed, and the
// read error — a killed download must not render as a fast healthy span.
func TestHTTPTransportBodyErrorIsReported(t *testing.T) {
	var events []Event
	hc := &http.Client{Transport: &HTTPTransport{Base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(&failingReader{data: []byte("abc")})}, nil
	})}}
	req, _ := http.NewRequestWithContext(WithHTTPClass(scopedHTTP(&events), HTTPTarballDownload), http.MethodGet, "https://cdn.example/x", nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("want mid-body read error")
	}
	resp.Body.Close()
	resp.Body.Close() // double close must not double-emit

	if len(events) != 1 {
		t.Fatalf("got %d events, want exactly 1", len(events))
	}
	h := events[0].HTTP
	if h.Status != http.StatusOK {
		t.Errorf("Status = %d, want the 200 the headers claimed", h.Status)
	}
	if h.Error == "" || !strings.Contains(h.Error, "stream torn") {
		t.Errorf("Error = %q, want the mid-body read error", h.Error)
	}
	if h.BytesRead != 3 {
		t.Errorf("BytesRead = %d, want 3", h.BytesRead)
	}
}

// slowReader yields its data after a delay, then EOF.
type slowReader struct {
	data  []byte
	delay time.Duration
	done  bool
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	r.done = true
	return copy(p, r.data), nil
}

// failingReader yields its data, then a non-EOF error.
type failingReader struct {
	data []byte
	done bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, errors.New("stream torn")
	}
	r.done = true
	return copy(p, r.data), nil
}
