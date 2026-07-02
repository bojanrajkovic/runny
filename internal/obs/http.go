package obs

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// KindHTTP is one HTTP round trip through an HTTPTransport, emitted at
// completion (response headers received, or a transport error). A single
// event, not a started/ended pair: HTTP calls repeat freely (retries, token
// refreshes, redirect hops), so there is no name-pairing contract to honor —
// consumers reconstruct the start as Time − Duration.
const KindHTTP Kind = "http"

// HTTPClass is the endpoint class of one round trip — the closed set of
// what a round trip may be called, same rule as action names: each becomes
// a span name ("http <class>") on the trace side, so an inline string would
// mint unbounded cardinality, and a URL path must never appear (paths carry
// org/repo; queries can carry credentials). The distinct type makes an
// inline literal require a visible conversion; add a constant here, never
// inline.
type HTTPClass string

const (
	HTTPGitHubToken          HTTPClass = "github.token"            // installation resolve + installation-token mint
	HTTPGitHubJIT            HTTPClass = "github.jit"              // generate-jitconfig
	HTTPGitHubRunnerList     HTTPClass = "github.runner-list"      // runner listing (teardown safety check, sweeps)
	HTTPGitHubRunnerDelete   HTTPClass = "github.runner-delete"    // runner deregistration
	HTTPGitHubRunnerDownload HTTPClass = "github.runner-downloads" // runner-tarball asset resolve
	HTTPRegistryToken        HTTPClass = "registry.token"          // registry bearer-token challenge
	HTTPRegistryManifest     HTTPClass = "registry.manifest"       // manifest GET (resolve)
	HTTPRegistryBlob         HTTPClass = "registry.blob"           // layer blob GET (scope-less from the pull actor)
	HTTPTarballDownload      HTTPClass = "tarball.download"        // the runner-tarball GET itself
	// HTTPOther is what an unannotated request through an HTTPTransport
	// reports as: still visible, never a raw URL. A round trip landing here
	// means a call site forgot its WithHTTPClass annotation.
	HTTPOther HTTPClass = "other"
)

// HTTPEvent is the payload for KindHTTP: one round trip's class, protocol
// facts, and duration. Host is req.URL.Hostname() — never the path or
// query. It is service-controlled at worst, not guest-controlled: usually
// the configured endpoint (api.github.com, the registry), but a redirect
// hop goes through RoundTrip again as its own event, so a CDN a service
// redirects to reports the CDN's hostname.
//
// Duration covers the whole exchange — request start to body completion
// (EOF or the caller's Close, whichever comes first) — so a multi-GiB
// download is a long event, not a fast one that "ended" at the headers;
// HeaderDuration marks when the headers arrived within that window, and
// BytesRead counts the body. Status is 0 when the round trip failed below
// HTTP (dial, TLS, deadline); Error then carries the transport-level error
// text, and Duration is start-to-failure with HeaderDuration zero. A body
// that dies mid-transfer keeps the Status the headers claimed and carries
// the read error in Error. Error text is safe to record: the *url.Error
// that embeds the full request URL is wrapped on by http.Client above
// RoundTrip, and body-read errors are stream-level, so neither carries a
// URL.
type HTTPEvent struct {
	Class          HTTPClass
	Method         string
	Host           string
	Status         int
	Error          string
	Duration       time.Duration
	HeaderDuration time.Duration
	BytesRead      int64
}

type httpClassKey struct{}

// WithHTTPClass annotates ctx with the endpoint class (one of the HTTP*
// constants above) that requests carrying this context report as. Call sites
// annotate rather than transports parsing URLs: the class is
// closed-by-construction and no code ever inspects a path or query to
// classify.
func WithHTTPClass(ctx context.Context, class HTTPClass) context.Context {
	return context.WithValue(ctx, httpClassKey{}, class)
}

// HTTPTransport is an http.RoundTripper that emits one KindHTTP event per
// round trip on requests whose context carries an obs scope. A scope-less
// request (the shared pull actor's blob traffic, startup doctor checks, any
// test) passes straight through to Base with no emitter work — the same
// degradation contract as Action. It adds visibility only: no retries, no
// header changes, no new network behavior.
type HTTPTransport struct {
	// Base is the underlying transport; nil means http.DefaultTransport.
	Base http.RoundTripper
}

func (t *HTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	ctx := req.Context()
	s := liveScope(ctx)
	if s == nil {
		return base.RoundTrip(req)
	}

	class, _ := ctx.Value(httpClassKey{}).(HTTPClass)
	if class == "" {
		class = HTTPOther
	}

	start := time.Now()
	resp, err := base.RoundTrip(req)

	h := HTTPEvent{
		Class:  class,
		Method: req.Method,
		Host:   req.URL.Hostname(),
	}
	if err != nil {
		h.Error = err.Error()
		h.Duration = time.Since(start)
		s.emitEvent(Event{Kind: KindHTTP, HTTP: &h})
		return resp, err
	}

	// The event waits for the body: RoundTrip returns at the headers, but the
	// exchange isn't over until the caller drains or closes resp.Body — for
	// the streaming classes (blobs, the tarball) that is where nearly all the
	// time and every byte lives. The wrapped body emits exactly once, at EOF
	// or Close, whichever comes first; http.Client itself closes the body of
	// each redirect hop, so no hop's event is lost.
	h.Status = resp.StatusCode
	h.HeaderDuration = time.Since(start)
	resp.Body = &observedBody{rc: resp.Body, emit: func(n int64, readErr error) {
		h.Duration = time.Since(start)
		h.BytesRead = n
		if readErr != nil {
			h.Error = readErr.Error()
		}
		s.emitEvent(Event{Kind: KindHTTP, HTTP: &h})
	}}
	return resp, nil
}

// observedBody counts a response body's bytes and reports its completion —
// EOF, a mid-stream read error, or the caller's Close — exactly once.
type observedBody struct {
	rc    io.ReadCloser
	emit  func(bytes int64, readErr error)
	once  sync.Once
	bytes int64
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.bytes += int64(n)
	if err != nil {
		readErr := err
		if errors.Is(err, io.EOF) {
			readErr = nil // a clean end, not a failure
		}
		b.finish(readErr)
	}
	return n, err
}

func (b *observedBody) Close() error {
	err := b.rc.Close()
	b.finish(nil)
	return err
}

func (b *observedBody) finish(readErr error) {
	b.once.Do(func() { b.emit(b.bytes, readErr) })
}
