# winhcs

Vendored slice of [`github.com/Microsoft/hcsshim`](https://github.com/Microsoft/hcsshim)
(MIT, see `LICENSE`) covering the HCS boot-path core and HNS endpoint
management -- the two APIs the Hyper-V VM backend (`internal/vm/hcs_windows.go`,
issue #308) needs to create/start/shut down/terminate a compute system and
attach it to a network endpoint. Vendored rather than taken as a normal
module dependency for two reasons: most of this code lives under hcsshim's
`internal/`, which Go's import-visibility rule makes unimportable from
outside the hcsshim module; and taking hcsshim as an ordinary dependency
pulls in logrus, containerd/errdefs, and a gRPC/protobuf stack this backend
has no other use for. This is a copy with rewritten import paths, trimmed
to what the boot path and endpoint management actually need.

**Origin:** `github.com/Microsoft/hcsshim` tag `v0.14.1`, commit
`fb5aa2e9478c8f5dcaba00601cc7c7d10e1320cd`.

Every file below is byte-identical to upstream except for import-path
rewrites and the modifications listed here. No other divergence is allowed --
upstream fixes should apply as a straight re-vendor.

**Import-path rewrite, including the flattened layout:** upstream
`github.com/Microsoft/hcsshim/internal/X` becomes
`github.com/bojanrajkovic/runny/internal/winhcs/X` -- one `internal/`, not
two. Upstream nests its boot-path packages a second `internal/` deep
specifically to hide them from *hcsshim's own* external consumers; that
restriction has no purpose once the tree lives under our own top-level
`internal/winhcs/`, and keeping it would actively break things here -- Go's
import-visibility rule would then block `internal/vm/hcs_windows.go` (outside
the `internal/winhcs` tree) from importing `hcs`/`hcs/schema2`/`vmcompute` at
all, since only code rooted at the parent of an `internal/` segment may
import through it. Collapsing the second `internal/` is a pure path change --
zero logic divergence -- but it must be repeated on every re-vendor, the same
as the import-path rewrite itself. `hcn`, `computestorage`, and `osversion`
were never nested this way upstream, so they're unaffected.

**The flatten has a Bazel-only half, easy to miss.** gazelle regenerates each
package's `importpath` on the Go path alone, but does not widen an existing
`visibility` attribute on its own -- the flatten PR left `hcs` and
`hcs/schema2` (and, latently, every other previously-nested package) at
gazelle's original `visibility = ["//internal/winhcs:__subpackages__"]`,
scoped for the OLD double-`internal/` layout. `go build`/`go vet` don't
enforce Bazel visibility at all, so this passed silently until
`internal/vm/hcs_windows.go` tried to depend on `hcs`/`hcs/schema2` through
Bazel and hit "target ... is not visible from target //internal/vm:vm". Widen
the specific package's `visibility` to `["//:__subpackages__"]` (matching
`hcn`/`computestorage`/`osversion`, which were never nested and so never had
this problem) the moment something outside `internal/winhcs` needs to depend
on it -- gazelle preserves a manually-widened `visibility` on later runs, it
just never widens one itself.

## Vendored packages

The package tree under this directory (`bazel query //internal/winhcs/...`,
or just `ls`) is the source of truth for what's vendored -- don't duplicate
it here, it drifts the moment a package is added or dropped. The one
exception worth calling out explicitly is `hcn`: it was folded in
separately from the rest, for #308's endpoint create/query needs (see
modification 5 below for exactly which of its files, and why not all of
them).

## Local modifications

1. **`internal/oc/errors.go`** -- replaced the containerd-errdefs/gRPC-codes
   error-to-span-status mapper with a ~20-line local one using
   `go.opencensus.io/trace`'s own `StatusCode*` constants (the same code
   space gRPC uses) instead of grpc's `codes.Code`. Same status semantics
   for the error classes the vendored `hcs` package actually produces
   (including `context.Canceled`/`context.DeadlineExceeded`, which
   `bounded.Context` deadlines and cancellations surface as); drops
   `containerd/errdefs`, `google.golang.org/grpc`, and the `genproto`/
   `protobuf` subtree those pull in.
2. **`internal/oc/exporter.go`** -- deleted. It's a logrus-flavored
   `trace.Exporter`; span export goes through the OpenCensus->OTel bridge
   instead (registered in #308, at runnyd init on windows).
3. **`internal/log`** -- rewritten onto stdlib `log/slog`, keeping the
   exported surface identical (`L`, `G(ctx)`, `Fields = map[string]any`,
   `Entry.WithField`/`WithFields`/`WithError` + the level methods the
   vendored tree calls) so every consumer needed only an import-identifier
   rename (`logrus.Fields` -> `log.Fields`, `logrus.WithFields` ->
   `log.WithFields`), no logic edits. Each level method checks
   `slog.Logger.Enabled` before building its message/fields, matching
   logrus's `IsLevelEnabled` short-circuit; `Trace` sits at a distinct level
   below `Debug` (`slog.LevelDebug - 4`), matching logrus's own ordering, so
   enabling Debug logging doesn't also turn on Trace-level noise. `Infof`
   and `Warnf` aren't ported -- nothing in the vendored tree calls them.
   `hook.go` (logrus `Hook` / span-annotation glue) and `nopformatter.go` (a
   `logrus.Formatter` stub) were dropped rather than ported: once
   `oc/exporter.go` is gone, nothing in this vendored slice calls either.
   `format.go`'s protobuf-`Any` pretty-printer went with them for the same
   reason (its only caller was `hook.go`); its two remaining live call sites
   (`internal/log/scrub.go`) used it purely to marshal plain Go structs/maps,
   not protobuf messages, so they now call `encoding/json.Marshal` directly.
4. **`internal/hcs/schema2/properties.go`** -- dropped the `Metrics
   *v1.Metrics` field (upstream: `LCOWMetrics`, sourced from
   `github.com/containerd/cgroups/v3/cgroup1/stats`). Nothing in this
   vendored slice reads it, and its type is genuine protobuf-generated code
   (`protoreflect`/`protoimpl` at compile time) -- keeping it would silently
   reintroduce `google.golang.org/protobuf` into the dependency graph,
   exactly what vendoring is meant to avoid.
5. **`hcn` slice** -- vendored only `hcn.go`, `hcnendpoint.go`,
   `hcnnetwork.go`, `hcnerrors.go`, `hcnglobals.go`, `hcnpolicy.go`,
   `hcnsupport.go`, `zsyscall_windows.go`, `doc.go`: everything the VM
   backend's endpoint create/query path needs. `hcnnamespace.go`,
   `hcnloadbalancer.go`, and `hcnroute.go` were left out -- they're
   container-namespace/load-balancer/route management, unrelated to a bare
   VM's network endpoint, and `hcnnamespace.go` specifically is the only
   file in the whole `hcn` package that imports `internal/cni`,
   `internal/regstate`, and `internal/runhcs` (containerd/runhcs-shim
   integration, well outside this backend's scope). Dropping it meant also
   dropping `HostComputeEndpoint.NamespaceAttach`/`NamespaceDetach` (two
   methods, `hcnendpoint.go`), the only callers of the namespace functions
   defined there.
6. All logrus imports across the remaining files (`internal/hcs/system.go`,
   `internal/hcs/callback.go`, `internal/vmcompute/vmcompute.go`,
   `internal/jobobject/iocp.go`, and the five `hcn/*.go` files above) were
   swapped for the `internal/log` shim: import-identifier rename only, per
   modification 3's exported-surface guarantee.

After these modifications, `sirupsen/logrus`, `containerd/errdefs`,
`containerd/typeurl`, `google.golang.org/grpc`, `google.golang.org/protobuf`,
and `google.golang.org/genproto` do not appear anywhere in
`bazel query 'deps(//internal/winhcs/...)'` for any platform.

## Module deps this PR adds

See `go.mod` for what this PR actually added -- it's the authority here, not
this doc. (`github.com/Microsoft/go-winio` was already a runny dependency
before this PR; its `vhd` subpackage is used by the differencing-disk clone
work, issue #306.)

**Deliberately not added here:** `go.opentelemetry.io/otel/bridge/opencensus`.
Nothing in this PR imports it -- the bridge is registered at runnyd init on
windows in #308, which is the PR that will add the real `go.mod` entry via
the normal `go mod tidy` workflow. Pre-adding an unimported module dependency
doesn't work well with this repo's tooling: `go mod tidy` prunes unused
`require` entries on every run (including Renovate's), so a dangling pin
would just get fought over for no benefit.

## Bazel

`internal/winhcs/...` is windows-only (every file carries `//go:build windows`
or a `_windows.go` suffix); gazelle emits a `select()` arm per target so
darwin/linux builds see an empty dep set. Building
`containerd/cgroups/v3` was going to need a `go_deps.gazelle_override`
(`gazelle:proto disable_global`) to make its checked-in `metrics.pb.go`
usable from Bazel at all -- moot now that modification 4 above removes the
dependency entirely, but worth knowing if a future PR re-introduces
`cgroup1/stats`.

`tools/lint/nogo_config.json` excludes exactly one file
(`internal/security/grantvmgroupaccess.go`) from exactly one analyzer
(`unsafeptr`, two pre-existing findings in byte-identical upstream code) --
scoped that narrowly on purpose, so the rest of `internal/winhcs/`,
including the hand-authored/modified files below, still gets full nogo
coverage.

`.gitattributes` marks `internal/winhcs/**` `linguist-generated=true` (with
the modified files below re-marked `false`) so `bazel run //tools/format`
leaves the untouched vendored tree's formatting alone -- upstream fixes
should apply as a clean re-vendor, not fight with gofumpt's opinions on
code this repo doesn't own.

## Tests

`internal/log`'s slog shim and `internal/oc`'s local error mapper each carry
a small, platform-independent unit test (`internal/log/context_test.go`,
`internal/oc/errors_test.go`) -- the two files with real logic changes, per
the modifications above. The vendored boot-path and `hcn` files carry none;
they are upstream code kept byte-identical.
