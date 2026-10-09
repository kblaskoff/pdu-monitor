import XCTest
@testable import PDUCore

private actor Log {
    var events: [String] = []
    func add(_ e: String) { events.append(e) }
}

private final class FakeController: OutletController, @unchecked Sendable {
    let lock = NSLock()
    var states: [OutletTarget: Bool] = [:]
    var failSet: Set<OutletTarget> = []
    var stuckOn: Set<OutletTarget> = []          // ignores OFF
    var blind: Set<OutletTarget> = []            // cannot report its state
    let log = Log()
    func set(_ target: OutletTarget, on: Bool) async throws {
        await log.add("\(on ? "ON" : "OFF") \(target.outlet)")
        if lock.withLock({ failSet.contains(target) }) { throw SNMPError.timeout }
        if !on && lock.withLock({ stuckOn.contains(target) }) { return }
        lock.withLock { states[target] = on }
    }
    func state(_ target: OutletTarget) async throws -> Bool? {
        lock.withLock { blind.contains(target) ? nil : states[target] }
    }
}

final class SequencerTests: XCTestCase {
    private let a = OutletTarget(pduID: UUID(), outlet: 1)
    private let b = OutletTarget(pduID: UUID(), outlet: 16)

    private func make(_ controller: FakeController, waits: Waits = Waits()) -> PowerSequencer {
        PowerSequencer(controller: controller, label: { "p\($0.outlet)" }, sleep: { seconds in await waits.add(seconds) },
                       confirmTimeout: 3, confirmInterval: 1)
    }
    actor Waits { var total: TimeInterval = 0; func add(_ s: TimeInterval) { total += s } }

    func testRestartSwitchesBothPortsOffWaitsThenBothOn() async throws {
        let controller = FakeController(); controller.states = [a: true, b: true]
        let waits = Waits()
        let progress = ProgressBox()
        try await make(controller, waits: waits).run(.restart(delay: 7), targets: [a, b]) { progress.add($0) }
        let events = await controller.log.events
        XCTAssertEqual(Set(events.prefix(2)), ["OFF 1", "OFF 16"])
        XCTAssertEqual(Set(events.suffix(2)), ["ON 1", "ON 16"])
        XCTAssertEqual(events.count, 4)
        let waited = await waits.total
        XCTAssertEqual(waited, 7)
        XCTAssertEqual(controller.states[a], true); XCTAssertEqual(controller.states[b], true)
        let seen = progress.all
        XCTAssertEqual(seen.first, .switchingOff)
        XCTAssertTrue(seen.contains(.confirmedOff)); XCTAssertTrue(seen.contains(.waiting(secondsLeft: 7))); XCTAssertEqual(seen.last, .confirmedOn)
    }

    func testFailedOffOnOnePDURestoresTheOther() async throws {
        let controller = FakeController(); controller.states = [a: true, b: true]; controller.failSet = [b]
        do { try await make(controller).run(.restart(delay: 5), targets: [a, b]); XCTFail("must throw") }
        catch let error as PowerError {
            guard case .switchOffFailed(let detail, let restored) = error else { return XCTFail("\(error)") }
            XCTAssertTrue(detail.contains("p16"))
            XCTAssertTrue(restored)
        }
        XCTAssertEqual(controller.states[a], true, "the port that was switched off is switched on again")
        let events = await controller.log.events
        XCTAssertTrue(events.contains("ON 1"))
        XCTAssertFalse(events.contains("ON 16"), "the port that failed to switch off was never touched, so it is not switched on")
    }

    func testOffThatThePDUDoesNotConfirmIsRolledBack() async throws {
        let controller = FakeController(); controller.states = [a: true, b: true]; controller.stuckOn = [b]
        do { try await make(controller).run(.restart(delay: 5), targets: [a, b]); XCTFail() }
        catch let error as PowerError {
            guard case .offNotConfirmed(let detail, _) = error else { return XCTFail("\(error)") }
            XCTAssertEqual(detail, "p16")
        }
        XCTAssertEqual(controller.states[a], true)
    }

    func testPortThatCannotReportItsStateDoesNotBlockTheRestart() async throws {
        let controller = FakeController(); controller.states = [a: true, b: true]; controller.blind = [b]
        try await make(controller).run(.restart(delay: 2), targets: [a, b])
        XCTAssertEqual(controller.states[b], true)
    }

    func testPlainOnAndOff() async throws {
        let controller = FakeController(); controller.states = [a: true]
        try await make(controller).run(.off, targets: [a])
        XCTAssertEqual(controller.states[a], false)
        try await make(controller).run(.on, targets: [a])
        XCTAssertEqual(controller.states[a], true)
    }

    func testFailedSwitchOnIsReported() async throws {
        let controller = FakeController(); controller.states = [a: false]; controller.failSet = [a]
        do { try await make(controller).run(.on, targets: [a]); XCTFail() }
        catch let error as PowerError { guard case .switchOnFailed = error else { return XCTFail("\(error)") } }
    }
}

private final class ProgressBox: @unchecked Sendable {
    private let lock = NSLock(); private var items: [PowerProgress] = []
    func add(_ p: PowerProgress) { lock.withLock { items.append(p) } }
    var all: [PowerProgress] { lock.withLock { items } }
}
