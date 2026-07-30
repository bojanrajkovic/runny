# internal/winhcs — AI Agent Notes

**Do not hand-edit this tree.** It is a vendored slice of
[`github.com/Microsoft/hcsshim`](https://github.com/Microsoft/hcsshim), kept
byte-identical to upstream except for an import-path rewrite and a short list
of deliberate local modifications. [`README.md`](README.md) is the vendor
authority: it names the upstream tag and commit, every modification and why,
and what the re-vendor procedure has to repeat. Read it before changing
anything here, and update it in the same commit as any change to the tree.

`internal/vm/hcs_windows.go` is the only consumer — it drives compute-system
create/start/shutdown/terminate and HNS endpoint attach. Everything under this
directory is windows-only (`//go:build windows` or a `_windows.go` suffix), so
a green `bazel test //...` on macOS has not compiled a line of it.

A bug in behavior shared with upstream is fixed by re-vendoring a newer
upstream, not by patching here — a local patch has to be re-applied, by hand,
on every future re-vendor, and the README's "no other divergence" claim is
what keeps that list short enough to be true.

## Sharp edges

- **A cross-build of `//internal/winhcs/...` must exclude two test targets.**
  `bazel build --platforms=@rules_go//go/toolchain:windows_amd64 //internal/winhcs/...`
  fails on `//internal/winhcs/log:log_test` and `//internal/winhcs/oc:oc_test`
  — cross-compiling a windows `go_test` needs a windows test-execution
  platform, which the macOS/Linux host doesn't have. Append
  `-//internal/winhcs/log:log_test -//internal/winhcs/oc:oc_test` (what the CI
  Linux job's cross-build step does). The windows CI job builds them for real,
  because it runs *on* windows.
- **Widening a package's Bazel `visibility` is a manual step gazelle will
  never do for you.** The vendored layout collapses upstream's second
  `internal/` segment, and gazelle regenerates each package's `importpath`
  from the Go path but leaves an existing `visibility` attribute alone. Every
  previously-nested package therefore still carries
  `visibility = ["//internal/winhcs:__subpackages__"]`, scoped for the old
  layout. `go build`/`go vet` don't enforce Bazel visibility at all, so this
  fails only at the moment something outside `internal/winhcs` first depends
  on the package — as `//internal/vm` did on `hcs`/`hcs/schema2`, with
  `target ... is not visible from target //internal/vm:vm`. Widen that
  package to `["//:__subpackages__"]` (matching `hcn` and `osversion`, never
  nested and so never affected); gazelle preserves a manual widening on later
  runs.
- **`bazel run //tools/format` deliberately skips this tree, so a hand-edited
  file stays unformatted and nogo still judges it.** `.gitattributes` marks
  `internal/winhcs/**` `linguist-generated=true` — the point is that an
  upstream fix applies as a clean re-vendor instead of fighting gofumpt over
  code this repo doesn't own. A file this repo *does* modify must be re-marked
  `linguist-generated=false` in the same commit, or it silently drops out of
  formatting forever. The nogo exclusion is separate and much narrower: one
  analyzer (`unsafeptr`) on one file
  (`security/grantvmgroupaccess.go`, two pre-existing upstream findings), in
  `tools/lint/nogo_config.json`. Don't widen it to cover a new hand-authored
  file — fix the finding.
- **`log.L` binds `slog.Default()` at package init, which flattens every
  vendored log line to INFO and drops Debug and Trace entirely.** Two review
  passes have read this as "winhcs logs never reach the pipeline", and that is
  wrong — the init-time default handler writes through the stdlib `log`
  package, which a later `slog.SetDefault` redirects, so the *messages* do
  arrive. What does not survive is the level: `Entry.enabled` is evaluated
  against the init-time handler, whose level is Info, so `Debug`/`Debugf` and
  `Trace` short-circuit to false before emitting, and everything that does
  emit lands as an INFO record. `vmcompute`'s stuck-syscall warning — the one
  line here worth waking up for — arrives looking routine. Resolving
  `slog.Default()` per call instead of once at init fixes the class; this is
  the shim's own code (a listed local modification), so it is the one place
  here a fix belongs rather than a re-vendor.
