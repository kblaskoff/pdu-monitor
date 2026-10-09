import Foundation

public enum PowerOperation: Equatable, Sendable {
    case on, off
    /// Off, wait `delay` seconds, on.
    case restart(delay: TimeInterval)
}

public enum PowerProgress: Equatable, Sendable {
    case switchingOff, confirmedOff
    case waiting(secondsLeft: Int)
    case switchingOn, confirmedOn
    case rolledBack
}

public enum PowerError: Error, LocalizedError, Equatable {
    case switchOffFailed(String, restored: Bool)
    case offNotConfirmed(String, restored: Bool)
    case switchOnFailed(String)
    case onNotConfirmed(String)
    public var errorDescription: String? {
        switch self {
        case .switchOffFailed(let detail, let restored):
            return "Could not switch off: \(detail). " + (restored ? "Ports that were already off were switched back on." : "")
        case .offNotConfirmed(let detail, let restored):
            return "Switch-off was not confirmed by the PDU: \(detail). " + (restored ? "The ports were switched back on." : "")
        case .switchOnFailed(let detail): return "Could not switch on: \(detail)"
        case .onNotConfirmed(let detail): return "Switch-on was not confirmed by the PDU: \(detail)"
        }
    }
}

public protocol OutletController: Sendable {
    func set(_ target: OutletTarget, on: Bool) async throws
    func state(_ target: OutletTarget) async throws -> Bool?
}

/// Switches the ports of one server (or of one port): the same steps for one port or for the two PDUs of a dual-feed server.
/// A restart is not the PDU's own "reboot" command (the two PDUs would not stay in step) but explicit OFF on all ports,
/// confirmation, a pause, then ON on all ports and confirmation. If a switch-off fails, the ports already turned off are turned on again.
public struct PowerSequencer: Sendable {
    public var controller: OutletController
    public var label: @Sendable (OutletTarget) -> String
    public var sleep: @Sendable (TimeInterval) async throws -> Void
    public var confirmTimeout: TimeInterval
    public var confirmInterval: TimeInterval

    public init(controller: OutletController, label: @escaping @Sendable (OutletTarget) -> String = { "\($0.pduID.uuidString.prefix(4)) #\($0.outlet)" },
                sleep: @escaping @Sendable (TimeInterval) async throws -> Void = { try await Task.sleep(nanoseconds: UInt64($0 * 1_000_000_000)) },
                confirmTimeout: TimeInterval = 10, confirmInterval: TimeInterval = 1) {
        self.controller = controller; self.label = label; self.sleep = sleep
        self.confirmTimeout = confirmTimeout; self.confirmInterval = confirmInterval
    }

    public func run(_ operation: PowerOperation, targets: [OutletTarget], progress: @escaping @Sendable (PowerProgress) -> Void = { _ in }) async throws {
        guard !targets.isEmpty else { return }
        switch operation {
        case .off:
            progress(.switchingOff)
            try await switchOff(targets, progress: progress)
        case .on:
            progress(.switchingOn)
            try await switchOn(targets, progress: progress)
        case .restart(let delay):
            progress(.switchingOff)
            try await switchOff(targets, progress: progress)
            var left = Int(delay.rounded(.up))
            while left > 0 {
                progress(.waiting(secondsLeft: left))
                try await sleep(1)
                left -= 1
            }
            progress(.switchingOn)
            try await switchOn(targets, progress: progress)
        }
    }

    // MARK: steps

    private func switchOff(_ targets: [OutletTarget], progress: @escaping @Sendable (PowerProgress) -> Void) async throws {
        let outcomes = await perform(targets, on: false)
        let failed = outcomes.filter { $0.error != nil }
        if !failed.isEmpty {
            let done = outcomes.filter { $0.error == nil }.map(\.target)
            let restored = await restore(done)
            if restored { progress(.rolledBack) }
            throw PowerError.switchOffFailed(describe(failed), restored: restored)
        }
        let notOff = await waitFor(targets, on: false)
        if !notOff.isEmpty {
            let restored = await restore(targets)
            if restored { progress(.rolledBack) }
            throw PowerError.offNotConfirmed(notOff.map(label).joined(separator: ", "), restored: restored)
        }
        progress(.confirmedOff)
    }

    private func switchOn(_ targets: [OutletTarget], progress: @escaping @Sendable (PowerProgress) -> Void) async throws {
        let outcomes = await perform(targets, on: true)
        let failed = outcomes.filter { $0.error != nil }
        if !failed.isEmpty { throw PowerError.switchOnFailed(describe(failed)) }
        let notOn = await waitFor(targets, on: true)
        if !notOn.isEmpty { throw PowerError.onNotConfirmed(notOn.map(label).joined(separator: ", ")) }
        progress(.confirmedOn)
    }

    private struct Outcome: Sendable { var target: OutletTarget; var error: String? }

    private func perform(_ targets: [OutletTarget], on: Bool) async -> [Outcome] {
        await withTaskGroup(of: Outcome.self) { group in
            for target in targets {
                group.addTask {
                    do { try await controller.set(target, on: on); return Outcome(target: target, error: nil) }
                    catch { return Outcome(target: target, error: error.localizedDescription) }
                }
            }
            var result: [Outcome] = []
            for await outcome in group { result.append(outcome) }
            return targets.compactMap { t in result.first { $0.target == t } }
        }
    }

    private func describe(_ failed: [Outcome]) -> String {
        failed.map { "\(label($0.target)): \($0.error ?? "")" }.joined(separator: "; ")
    }

    /// The targets that did not reach the wanted state in time. A PDU that cannot tell its state (nil) is not waited for.
    private func waitFor(_ targets: [OutletTarget], on: Bool) async -> [OutletTarget] {
        var pending = targets
        var waited: TimeInterval = 0
        while true {
            var still: [OutletTarget] = []
            for target in pending {
                if let state = try? await controller.state(target), state != on { still.append(target) }
            }
            pending = still
            if pending.isEmpty || waited >= confirmTimeout { return pending }
            try? await sleep(confirmInterval)
            waited += confirmInterval
        }
    }

    /// Switch back on what was switched off. True when it was tried for at least one port and none failed.
    private func restore(_ targets: [OutletTarget]) async -> Bool {
        guard !targets.isEmpty else { return false }
        return await perform(targets, on: true).allSatisfy { $0.error == nil }
    }
}
