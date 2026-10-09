import XCTest
@testable import PDUCore

final class RackTests: XCTestCase {
    private func pdu(_ name: String, rack: UUID, outlets: [OutletReading], amps: Double?, watts: Double? = nil, limit: Double = 24) -> PDUState {
        let snapshot = PDUSnapshot(info: PDUInfo(vendor: .cyberPower, outletCount: outlets.count), outlets: outlets, totalAmps: amps, totalWatts: watts)
        return PDUState(config: DeviceConfig(name: name, rackID: rack, vendor: .cyberPower, host: name, maxAmps: limit), snapshot: snapshot)
    }

    func testOutletNames() {
        for name in ["", "  ", "Outlet_7", "Outlet 7", "outlet-07", "OUTLET7", "Outlet"] { XCTAssertTrue(ServerID.isDefaultOutletName(name), name) }
        for name in ["200U31", "200U12-1", "N200U43", "Outlet_main", "a41sf"] { XCTAssertFalse(ServerID.isDefaultOutletName(name), name) }
        XCTAssertEqual(ServerID.kind(of: "N200U43"), .network)
        XCTAssertEqual(ServerID.kind(of: "200U43"), .server)
        XCTAssertEqual(ServerID.kind(of: "NAS01"), .server)
        XCTAssertEqual(ServerID.kind(of: "n12u3"), .network)
        XCTAssertEqual(ServerID.key(" 200u31 "), "200u31")
    }

    func testTwoPDUsAreJoinedIntoOneServer() {
        let rack = UUID()
        let a = pdu("P200A", rack: rack, outlets: [
            OutletReading(number: 1, name: "200U31", isOn: true, amps: 0.3, watts: 47),
            OutletReading(number: 2, name: "Outlet_2", isOn: true, amps: 0, watts: 0),
            OutletReading(number: 3, name: "N200U43", isOn: true, amps: 0.3, watts: 64),
            OutletReading(number: 4, name: "200U9", isOn: true, amps: 1, watts: 100)], amps: 3)
        let b = pdu("P200B", rack: rack, outlets: [
            OutletReading(number: 1, name: "200u31", isOn: true, amps: 0.2, watts: 10),
            OutletReading(number: 2, name: "200U9", isOn: false, amps: 0, watts: 0)], amps: 1)
        let servers = RackAggregator.servers(of: [a, b])
        XCTAssertEqual(servers.map(\.name), ["200U9", "200U31", "N200U43"])      // natural order: 9 before 31
        let u31 = servers[1]
        XCTAssertEqual(u31.ports.count, 2)
        XCTAssertEqual(u31.ports.map(\.pduName), ["P200A", "P200B"])
        XCTAssertEqual(u31.watts, 57)
        XCTAssertEqual(u31.amps ?? 0, 0.5, accuracy: 0.001)
        XCTAssertEqual(u31.power, .on)
        XCTAssertEqual(servers[0].power, .mixed)                                  // one port on, one off
        XCTAssertEqual(servers[2].kind, .network)
        XCTAssertEqual(u31.targets.count, 2)
    }

    func testUnmeteredServerHasNoWatts() {
        let rack = UUID()
        let a = pdu("P", rack: rack, outlets: [OutletReading(number: 1, name: "S1", isOn: true)], amps: 5)
        XCTAssertNil(RackAggregator.servers(of: [a])[0].watts)
    }

    func testRackSummaryAndLimits() {
        let rackConfig = RackConfig(name: "200", maxAmps: 24)
        let a = pdu("A", rack: rackConfig.id, outlets: [], amps: 12.8, watts: 2329)
        let b = pdu("B", rack: rackConfig.id, outlets: [], amps: 11.2, watts: 2150)
        var summary = RackSummarizer.summary(rack: rackConfig, pdus: [a, b])
        XCTAssertEqual(summary.amps ?? 0, 24.0, accuracy: 0.001)
        XCTAssertEqual(summary.watts, 4479)
        XCTAssertEqual(summary.status, .warning)           // 24.0 of 24: at the limit, not over it
        let c = pdu("C", rack: rackConfig.id, outlets: [], amps: 0.5)
        summary = RackSummarizer.summary(rack: rackConfig, pdus: [a, b, c])
        XCTAssertEqual(summary.status, .over)
        // a single PDU over its own limit is noticed even when the rack total is fine
        let big = RackConfig(name: "big", maxAmps: 48)
        let over = pdu("O", rack: big.id, outlets: [], amps: 25, limit: 24)
        let fine = pdu("F", rack: big.id, outlets: [], amps: 5, limit: 24)
        let bigSummary = RackSummarizer.summary(rack: big, pdus: [over, fine])
        XCTAssertEqual(bigSummary.status, .ok)
        XCTAssertEqual(bigSummary.worstPDU, .over)
        XCTAssertEqual(bigSummary.attention, .over)
    }

    func testNoReadingsMeansUnknownNotZero() {
        let rack = RackConfig(name: "x")
        let offline = PDUState(config: DeviceConfig(name: "A", rackID: rack.id, vendor: .apc, host: "h"), snapshot: nil, error: "timeout")
        let summary = RackSummarizer.summary(rack: rack, pdus: [offline])
        XCTAssertNil(summary.amps)
        XCTAssertEqual(summary.status, .unknown)
        XCTAssertEqual(summary.attention, .unknown)
        XCTAssertEqual(summary.offlineCount, 1)
    }

    func testLimitStatusBoundaries() {
        XCTAssertEqual(LimitStatus.evaluate(amps: 19.1, limit: 24), .ok)
        XCTAssertEqual(LimitStatus.evaluate(amps: 19.2, limit: 24), .warning)
        XCTAssertEqual(LimitStatus.evaluate(amps: 24, limit: 24), .warning)
        XCTAssertEqual(LimitStatus.evaluate(amps: 24.1, limit: 24), .over)
        XCTAssertEqual(LimitStatus.evaluate(amps: nil, limit: 24), .unknown)
        XCTAssertEqual(LimitStatus.evaluate(amps: 5, limit: 0), .unknown)
    }

    func testSnapshotTotalsFallBack() {
        let outlets = [OutletReading(number: 1, name: "a", amps: 1.5, watts: 300), OutletReading(number: 2, name: "b", amps: 0.5, watts: 100)]
        let bare = PDUSnapshot(info: PDUInfo(vendor: .apc), outlets: outlets)
        XCTAssertEqual(bare.totalAmps ?? 0, 2.0, accuracy: 0.001)
        XCTAssertEqual(bare.totalWatts, 400)
        let withBanks = PDUSnapshot(info: PDUInfo(vendor: .apc), outlets: outlets, banks: [BankReading(number: 1, amps: 3), BankReading(number: 2, amps: 4)])
        XCTAssertEqual(withBanks.totalAmps ?? 0, 7, accuracy: 0.001)
    }

    func testConfigKeepsCommunitiesOutOfJSON() throws {
        let device = DeviceConfig(name: "A", rackID: UUID(), vendor: .apc, host: "10.0.0.5", readCommunity: "secret-read", writeCommunity: "secret-write")
        let json = String(decoding: try JSONEncoder().encode(device), as: UTF8.self)
        XCTAssertFalse(json.contains("secret"))
        let back = try JSONDecoder().decode(DeviceConfig.self, from: Data(json.utf8))
        XCTAssertEqual(back.host, "10.0.0.5")
        XCTAssertEqual(back.readCommunity, "")
        XCTAssertEqual(back.port, 161)
    }

    func testServerLabels() {
        var labels = ServerLabels()
        let rack = UUID()
        labels.set("  web-1 ", rack: rack, id: "200U31")
        XCTAssertEqual(labels.label(rack: rack, id: "200u31"), "web-1")
        labels.set("", rack: rack, id: "200U31")
        XCTAssertNil(labels.label(rack: rack, id: "200U31"))
    }
}

final class DemoLabTests: XCTestCase {
    func testDemoRacksShowEveryStatusAndTheUnmeteredPDUReportsItsLoad() async throws {
        let lab = DemoLab.make()
        var statuses: [String: LimitStatus] = [:]
        for rack in lab.racks {
            var pdus: [PDUState] = []
            for config in lab.devices where config.rackID == rack.id {
                pdus.append(PDUState(config: config, snapshot: try await lab.drivers[config.id]!.poll()))
            }
            statuses[rack.name] = RackSummarizer.summary(rack: rack, pdus: pdus).status
            // the unmetered APC (first PDU of each rack) has no outlet figures but does report the whole-unit load
            XCTAssertNil(pdus[0].snapshot?.outlets[0].amps)
            XCTAssertGreaterThan(pdus[0].amps ?? 0, 1)
        }
        XCTAssertEqual(statuses["100"], .ok)
        XCTAssertEqual(statuses["200"], .warning)
        XCTAssertEqual(statuses["20F"], .over)
    }

    func testSimulatedRestartThroughTheSequencer() async throws {
        let lab = DemoLab.make()
        let id = lab.devices[1].id
        struct Controller: OutletController {
            let driver: PDUDriver
            func set(_ t: OutletTarget, on: Bool) async throws { try await driver.setOutlet(t.outlet, on: on) }
            func state(_ t: OutletTarget) async throws -> Bool? { try await driver.outletState(t.outlet) }
        }
        let sequencer = PowerSequencer(controller: Controller(driver: lab.drivers[id]!), sleep: { _ in })
        try await sequencer.run(.restart(delay: 5), targets: [OutletTarget(pduID: id, outlet: 1)])
        let state = try await lab.drivers[id]!.outletState(1)
        XCTAssertEqual(state, true)
    }
}
