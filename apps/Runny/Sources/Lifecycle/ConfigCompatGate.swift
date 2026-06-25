import Foundation

/// The Swift side of runnyd's config-compat gate (`runnyd -test-config`): exec the
/// bundled `runnyd` against the in-place config and parse its JSON verdict, so a
/// daemon update can be gated on whether the NEW binary accepts the CURRENT config —
/// the question the running daemon's reload preflight (old schema) cannot answer.
///
/// Same split as the rest of the lifecycle layer: `parseVerdict` is the pure,
/// unit-tested surface; `probe` is the thin live-only shell over `BoundedProcess`.

/// The verdict `runnyd -test-config` emits — the cross-language contract, mirrored
/// from the Go `configVerdict`. The JSON is the contract; this Decodable tracks it.
struct ConfigCompatVerdict: Decodable, Equatable {
    enum Status: String, Decodable {
        case ok
        case warn
        case error
    }

    let status: Status
    let errors: [String]
    let warnings: [Warning]

    struct Warning: Decodable, Equatable {
        let kind: String
        let message: String
    }
}

/// The outcome of running the gate: a parsed verdict, or `unavailable` when the
/// probe couldn't run or its output couldn't be parsed. `unavailable` is NOT a
/// silent pass — the update flow must treat it as blocking, never as OK: a gate
/// that can't speak must not green-light an upgrade.
enum ConfigCompatResult: Equatable {
    case verdict(ConfigCompatVerdict)
    case unavailable(String)
}

enum ConfigCompatGate {
    /// Generous: the checks are local (parse/validate/guest-cap/namespace/image-ref)
    /// with no network, but a cold exec plus JSON encode shouldn't be clipped. A hang
    /// past this surfaces as `unavailable` (blocking), never a spin.
    static let probeTimeout: Duration = .seconds(10)
    static let stdoutByteCap = 256 * 1024

    /// Run the bundled `runnyd -test-config <configPath>` and parse the verdict. The
    /// thin live-only shell; `parseVerdict` is the pure surface.
    static func probe(runnydPath: String, configPath: String) async -> ConfigCompatResult {
        switch await BoundedProcess.run(
            runnydPath, ["-test-config", configPath],
            timeout: probeTimeout, stdoutByteCap: stdoutByteCap
        ) {
        case let .exited(_, stdout, stderr):
            // The exit code mirrors the status (non-zero on error), but the JSON is
            // the contract and is printed in every case — so parse stdout regardless
            // of the code, and only fall back to stderr when there's no verdict.
            let parsed = parseVerdict(stdout)
            if case .unavailable = parsed {
                let detail = stderr.trimmingCharacters(in: .whitespacesAndNewlines)
                return .unavailable(detail.isEmpty ? "runnyd -test-config produced no parseable verdict" : detail)
            }
            return parsed
        case .timedOut:
            return .unavailable("runnyd -test-config timed out")
        case let .launchFailed(message):
            return .unavailable(message)
        }
    }

    /// Parse a `-test-config` verdict from runnyd's stdout. Pure and total: any
    /// non-decodable output (including an unmodeled status) is `unavailable`, never
    /// a fabricated OK.
    static func parseVerdict(_ stdout: String) -> ConfigCompatResult {
        guard let data = stdout.data(using: .utf8),
              let verdict = try? JSONDecoder().decode(ConfigCompatVerdict.self, from: data)
        else {
            return .unavailable("runnyd -test-config produced no parseable verdict")
        }
        return .verdict(verdict)
    }

    /// What a daemon update does with a gate result — the OK/Warn/Error decision of
    /// the update flow. Pure and total: OK proceeds; Warn proceeds only behind a
    /// manual confirmation that surfaces the warnings; Error and `unavailable`
    /// block. A gate that returns an error or can't speak must never reload — a
    /// schema-incompatible upgrade is blocked here, not drained into a crash-loop.
    enum UpdateGate: Equatable {
        case proceed
        case confirm([ConfigCompatVerdict.Warning])
        case block(String)
    }

    static func updateGate(for result: ConfigCompatResult) -> UpdateGate {
        switch result {
        case let .verdict(v):
            switch v.status {
            case .ok:
                return .proceed
            case .warn:
                return .confirm(v.warnings)
            case .error:
                let detail = v.errors.isEmpty ? "the new runnyd rejects the current config" : v.errors.joined(separator: "; ")
                return .block(detail)
            }
        case let .unavailable(why):
            return .block("couldn't verify the config against the new runnyd: \(why)")
        }
    }

    /// The re-gate decision at the *confirmed* reload, given the commit-time verdict
    /// and the warnings the operator already confirmed at the click. Proceed on OK,
    /// or on a Warn whose warnings the operator already saw and confirmed; otherwise
    /// don't silently apply an unseen verdict — `block` a hard incompatibility, or
    /// `reconfirm` a changed/new Warn so its warnings surface and the operator
    /// re-confirms. The click-time gate and this both go through `updateGate`, so the
    /// only new judgment here is "is this the same Warn they already confirmed?".
    enum CommitGate: Equatable {
        case proceed
        case block(String)
        case reconfirm([ConfigCompatVerdict.Warning])
    }

    static func commitGate(_ verdict: UpdateGate, confirmedWarnings: [ConfigCompatVerdict.Warning]) -> CommitGate {
        switch verdict {
        case .proceed:
            .proceed
        case let .block(message):
            .block(message)
        case let .confirm(warnings):
            warnings == confirmedWarnings ? .proceed : .reconfirm(warnings)
        }
    }
}
