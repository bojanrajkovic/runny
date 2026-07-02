package obs

import (
	"context"
	"net/http"
	"time"
)

// KindHTTP is one HTTP round trip through an HTTPTransport, emitted at
// completion (response headers received, or a transport error). A single
// event, not a started/ended pair: HTTP calls repeat freely (retries, token
// refreshes, redirect hops), so there is no name-pairing contract to honor —
// consumers reconstruct the start as Time − Duration.
const KindHTTP Kind = "http"

// HTTP endpoint classes — the closed set of what a round trip may be called,
// same rule as action names: each becomes a span name ("http <class>") on
// the trace side, so an inline string would mint unbounded cardinality, and
// a URL path must never appear (paths carry org/repo; queries can carry
// credentials). Add here, never inline.
const (
	HTTPGitHubToken          = "github.token"            // installation resolve + installation-token mint
	HTTPGitHubJIT            = "github.jit"              // generate-jitconfig
	HTTPGitHubRunnerList     = "github.runner-list"      // runner listing (teardown safety check, sweeps)
	HTTPGitHubRunnerDelete   = "github.runner-delete"    // runner deregistration
	HTTPGitHubRunnerDownload = "github.runner-downloads" // runner-tarball asset resolve
	HTTPRegistryToken        = "registry.token"          // registry bearer-token challenge
	HTTPRegistryManifest     = "registry.manifest"       // manifest GET (resolve)
	HTTPRegistryBlob         = "registry.blob"           // layer blob GET (scope-less from the pull actor)
	HTTPTarballDownload      = "tarball.download"        // the runner-tarball GET itself
	// HTTPOther is what an unannotated request through an HTTPTransport
	// reports as: still visible, never a raw URL. A round trip landing here
	// means a call site forgot its WithHTTPClass annotation.
	HTTPOther = "other"
)

// HTTPEvent is the payload for KindHTTP: one round trip's class, protocol
// facts, and duration. Host is req.URL.Hostname() — config-controlled
// (api.github.com, the registry, the tarball CDN), never the path or query.
// Status is 0 when the round trip failed below HTTP (dial, TLS, deadline);
// Error then carries the transport-level error text. That text is safe to
// record: the *url.Error that embeds the full request URL is wrapped on by
// http.Client above RoundTrip, so a RoundTripper never sees it.
type HTTPEvent struct {
	Class    string
	Method   string
	Host     string
	Status   int
	Error    string
	Duration time.Duration
}

type httpClassKey struct{}

// WithHTTPClass annotates ctx with the endpoint class (one of the HTTP*
// constants above) that requests carrying this context report as. Call sites
// annotate rather than transports parsing URLs: the class is
// closed-by-construction and no code ever inspects a path or query to
// classify.
func WithHTTPClass(ctx context.Context, class string) context.Context {
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
	if s, _ := ctx.Value(scopeKey{}).(*scope); s == nil || s.emit == nil {
		return base.RoundTrip(req)
	}

	class, _ := ctx.Value(httpClassKey{}).(string)
	if class == "" {
		class = HTTPOther
	}

	start := time.Now()
	resp, err := base.RoundTrip(req)

	h := &HTTPEvent{
		Class:    class,
		Method:   req.Method,
		Host:     req.URL.Hostname(),
		Duration: time.Since(start),
	}
	if err != nil {
		h.Error = err.Error()
	} else {
		h.Status = resp.StatusCode
	}
	Emit(ctx, Event{Kind: KindHTTP, HTTP: h})

	return resp, err
}
