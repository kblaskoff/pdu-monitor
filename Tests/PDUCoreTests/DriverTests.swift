import XCTest
@testable import PDUCore

/// The OIDs here are typed out from the vendor MIB files (CPS-MIB and PowerNet-MIB), on purpose not shared with the drivers.
final class DriverTests: XCTestCase {
    private let cps = "1.3.6.1.4.1.3808.1.1.6"
    private let apc = "1.3.6.1.4.1.318.1.1"

    /// P200B of the sample screen: 16 outlets, 2 banks, 230 V.
    private func makeCyberPower() throws -> FakeAgent {
        let agent = try FakeAgent()
        agent["\(cps).2.2.1.9.1"] = .octetString(Array("PDU81007".utf8))
        agent["\(cps).2.2.1.3.1"] = .octetString(Array("P200B".utf8))
        agent["\(cps).3.3.1.5.1"] = .integer(16)
        agent["\(cps).3.3.1.8.1"] = .integer(1)
        agent["\(cps).3.3.1.9.1"] = .integer(2)
        agent["\(cps).3.3.1.11.1"] = .integer(1)
        agent["\(cps).3.4.1.5.1"] = .unsigned(112)
        agent["\(cps).3.4.1.18.1"] = .unsigned(2150)
        agent["\(cps).4.4.1.5.1"] = .unsigned(112)
        agent["\(cps).4.4.1.7.1"] = .unsigned(2150)
        agent["\(cps).4.4.1.13.1"] = .unsigned(2300)
        agent["\(cps).5.4.1.5.1"] = .unsigned(65)
        agent["\(cps).5.4.1.5.2"] = .unsigned(47)
        let names = ["200U31", "200U24", "200U16", "200U29", "200U27", "200U18", "Outlet_7", "200U3",
                     "200U2", "200U34", "200U22", "200U12-1", "Outlet_13", "200U13", "Outlet_15", "200U20"]
        let tenthsAmps = [2, 5, 18, 5, 5, 20, 0, 10, 0, 4, 5, 4, 0, 15, 0, 19]
        let watts = [10, 80, 360, 100, 100, 390, 0, 200, 0, 80, 90, 70, 0, 300, 0, 370]
        for i in 1...16 {
            agent["\(cps).6.1.4.1.4.\(i)"] = .octetString(Array(names[i - 1].utf8))
            agent["\(cps).6.1.4.1.5.\(i)"] = .integer(1)                       // on
            agent["\(cps).6.1.5.1.5.\(i)"] = .integer(1)                       // command (reads as the state)
            agent["\(cps).6.1.3.1.6.\(i)"] = .integer(i <= 8 ? 1 : 2)
            agent["\(cps).6.2.4.1.6.\(i)"] = .unsigned(UInt64(tenthsAmps[i - 1]))
            agent["\(cps).6.2.4.1.7.\(i)"] = .unsigned(UInt64(watts[i - 1]))
        }
        agent.onSet = { oid, value, agent in
            // Control command OID .6.1.5.1.5.<i> mirrors into the status OID .6.1.4.1.5.<i>
            let c = oid.components
            if c.count >= 3, Array(c.dropLast().suffix(5)) == [6, 1, 5, 1, 5], let i = c.last {
                agent.set(OID("1.3.6.1.4.1.3808.1.1.6.6.1.4.1.5.\(i)")!, value)
            }
        }
        return agent
    }

    func testCyberPowerSnapshotMatchesTheSampleScreen() async throws {
        let agent = try makeCyberPower(); defer { agent.stop() }
        let snapshot = try await CyberPowerDriver(transport: agent.transport).poll()
        XCTAssertEqual(snapshot.info.model, "PDU81007")
        XCTAssertEqual(snapshot.info.name, "P200B")
        XCTAssertEqual(snapshot.info.outletCount, 16)
        XCTAssertEqual(snapshot.info.breakerCount, 2)
        XCTAssertEqual(snapshot.info.orientation, "Horizontal")
        XCTAssertEqual(snapshot.info.lineVoltage ?? 0, 230, accuracy: 0.01)
        XCTAssertEqual(snapshot.outlets.count, 16)
        XCTAssertEqual(snapshot.outlets[2].name, "200U16")
        XCTAssertEqual(snapshot.outlets[2].amps ?? 0, 1.8, accuracy: 0.001)
        XCTAssertEqual(snapshot.outlets[2].watts, 360)
        XCTAssertEqual(snapshot.outlets[2].isOn, true)
        XCTAssertEqual(snapshot.outlets[2].bank, 1)
        XCTAssertEqual(snapshot.outlets[15].bank, 2)
        XCTAssertFalse(snapshot.outlets[6].isAssigned)          // Outlet_7
        XCTAssertEqual(snapshot.totalAmps ?? 0, 11.2, accuracy: 0.001)
        XCTAssertEqual(snapshot.totalWatts, 2150)
        XCTAssertEqual(snapshot.banks.map(\.amps), [6.5, 4.7])
        XCTAssertTrue(snapshot.hasOutletMetering)
        // bank watts come from the outlets: 10+80+360+100+100+390+0+200 = 1240, the rest 910
        XCTAssertEqual(snapshot.banks.map(\.watts), [1240, 910])
    }

    func testCyberPowerSwitchingUsesTheWriteCommunityAndTheRightValues() async throws {
        let agent = try makeCyberPower(); defer { agent.stop() }
        let driver = CyberPowerDriver(transport: agent.transport)
        try await driver.setOutlet(3, on: false)
        XCTAssertEqual(agent["\(cps).6.1.5.1.5.3"], .integer(2))
        let offState = try await driver.outletState(3)
        XCTAssertEqual(offState, false)
        try await driver.setOutlet(3, on: true)
        let onState = try await driver.outletState(3)
        XCTAssertEqual(onState, true)
        let sets = agent.receivedRequests.filter { $0.kind == .set }
        XCTAssertEqual(sets.map(\.community), ["private", "private"])
    }

    func testReadOnlyCommunityCannotSwitch() async throws {
        let agent = try makeCyberPower(); defer { agent.stop() }
        let client = UDPSNMPClient(host: "127.0.0.1", port: agent.port, readCommunity: "public", writeCommunity: nil, timeout: 0.3, retries: 0)
        do { try await CyberPowerDriver(transport: client).setOutlet(1, on: false); XCTFail("should not work") }
        catch { XCTAssertEqual(error as? SNMPError, .timeout) }       // the agent ignores a SET with the wrong community
    }

    func testAnUnknownDeviceIsReportedAsNotRecognized() async throws {
        let agent = try FakeAgent(); defer { agent.stop() }
        agent["1.3.6.1.2.1.1.1.0"] = .octetString(Array("something else".utf8))
        do { _ = try await CyberPowerDriver(transport: agent.transport).poll(); XCTFail() }
        catch { XCTAssertTrue(error.localizedDescription.contains("CyberPower")) }
        do { _ = try await APCDriver(transport: agent.transport).poll(); XCTFail() }
        catch { XCTAssertTrue(error.localizedDescription.contains("APC")) }
    }

    func testNoAnswerIsATimeout() async throws {
        let client = UDPSNMPClient(host: "127.0.0.1", port: 9, readCommunity: "public", timeout: 0.3, retries: 0)   // nothing listens
        do { _ = try await client.get([OID("1.3.6.1.2.1.1.1.0")!]); XCTFail() }
        catch { XCTAssertNotNil(error as? SNMPError) }
    }

    // MARK: APC

    /// AP7932-like: 24 outlets, 2 banks, switched but only metered per bank (no outlet metering), rPDU2 firmware.
    private func makeAPC2() throws -> FakeAgent {
        let agent = try FakeAgent()
        agent["\(apc).26.2.1.3.1"] = .octetString(Array("P200A".utf8))
        agent["\(apc).26.2.1.8.1"] = .octetString(Array("AP7932".utf8))
        agent["\(apc).26.4.2.1.4.1"] = .integer(24)
        agent["\(apc).26.4.2.1.7.1"] = .integer(1)
        agent["\(apc).26.4.2.1.8.1"] = .integer(2)
        agent["\(apc).26.4.2.1.10.1"] = .integer(2)
        agent["\(apc).26.4.3.1.5.1"] = .integer(-1)
        agent["\(apc).26.6.3.1.5.1"] = .integer(128)
        agent["\(apc).26.6.3.1.6.1"] = .integer(230)
        agent["\(apc).26.6.3.1.7.1"] = .integer(233)           // hundredths of kW = 2330 W
        agent["\(apc).26.8.3.1.5.1"] = .integer(55)
        agent["\(apc).26.8.3.1.5.2"] = .integer(73)
        for i in 1...24 {
            agent["\(apc).26.9.2.3.1.3.\(i)"] = .octetString(Array((i == 1 ? "200U31" : "Outlet_\(i)").utf8))
            agent["\(apc).26.9.2.3.1.5.\(i)"] = .integer(2)             // on (in this MIB 1 is off!)
            agent["\(apc).26.9.2.2.1.6.\(i)"] = .integer(i <= 12 ? 1 : 2)
            agent["\(apc).26.9.2.4.1.5.\(i)"] = .integer(1)
        }
        agent.onSet = { oid, value, agent in
            // Control command OID .9.2.4.1.5.<i> mirrors into the status OID .9.2.3.1.5.<i> (here 2 is on and 1 is off)
            let c = oid.components
            if Array(c.dropLast().suffix(5)) == [9, 2, 4, 1, 5], let i = c.last {
                agent.set(OID("1.3.6.1.4.1.318.1.1.26.9.2.3.1.5.\(i)")!, .integer(value.intValue == 1 ? 2 : 1))
            }
        }
        return agent
    }

    func testAPCRPDU2SnapshotAndSwitching() async throws {
        let agent = try makeAPC2(); defer { agent.stop() }
        let driver = APCDriver(transport: agent.transport)
        let snapshot = try await driver.poll()
        XCTAssertEqual(snapshot.info.model, "AP7932")
        XCTAssertEqual(snapshot.outlets.count, 24)
        XCTAssertEqual(snapshot.outlets[0].name, "200U31")
        XCTAssertEqual(snapshot.outlets[0].isOn, true)
        XCTAssertNil(snapshot.outlets[0].amps)                  // unmetered: nil, not 0
        XCTAssertNil(snapshot.outlets[0].watts)
        XCTAssertFalse(snapshot.hasOutletMetering)
        XCTAssertEqual(snapshot.totalAmps ?? 0, 12.8, accuracy: 0.001)
        XCTAssertEqual(snapshot.totalWatts, 2330)               // from the phase: the device-level watts said -1
        XCTAssertEqual(snapshot.banks.map(\.amps), [5.5, 7.3])
        XCTAssertEqual(snapshot.outlets[23].bank, 2)
        try await driver.setOutlet(1, on: false)
        let off = try await driver.outletState(1)
        XCTAssertEqual(off, false)
        try await driver.setOutlet(1, on: true)
        let on = try await driver.outletState(1)
        XCTAssertEqual(on, true)
    }

    func testAPCLegacyFirmware() async throws {
        let agent = try FakeAgent(); defer { agent.stop() }
        agent["\(apc).12.1.5.0"] = .octetString(Array("AP7932".utf8))
        agent["\(apc).12.1.8.0"] = .integer(24)
        agent["\(apc).12.1.9.0"] = .integer(1)
        // load table: phase 1 then banks 1 and 2
        agent["\(apc).12.2.3.1.1.2.1"] = .unsigned(128); agent["\(apc).12.2.3.1.1.4.1"] = .integer(1); agent["\(apc).12.2.3.1.1.5.1"] = .integer(0)
        agent["\(apc).12.2.3.1.1.2.2"] = .unsigned(55); agent["\(apc).12.2.3.1.1.4.2"] = .integer(1); agent["\(apc).12.2.3.1.1.5.2"] = .integer(1)
        agent["\(apc).12.2.3.1.1.2.3"] = .unsigned(73); agent["\(apc).12.2.3.1.1.4.3"] = .integer(1); agent["\(apc).12.2.3.1.1.5.3"] = .integer(2)
        for i in 1...24 {
            agent["\(apc).4.4.2.1.4.\(i)"] = .octetString(Array((i == 2 ? "200U29" : "Outlet \(i)").utf8))
            agent["\(apc).4.4.2.1.3.\(i)"] = .integer(1)
        }
        agent.onSet = { oid, value, agent in agent.set(oid, value) }
        let driver = APCDriver(transport: agent.transport)
        let snapshot = try await driver.poll()
        XCTAssertEqual(snapshot.outlets.count, 24)
        XCTAssertEqual(snapshot.outlets[1].name, "200U29")
        XCTAssertEqual(snapshot.phases.count, 1)
        XCTAssertEqual(snapshot.banks.map(\.amps), [5.5, 7.3])
        XCTAssertEqual(snapshot.totalAmps ?? 0, 12.8, accuracy: 0.001)
        try await driver.setOutlet(2, on: false)
        XCTAssertEqual(agent["\(apc).4.4.2.1.3.2"], .integer(2))
        let state = try await driver.outletState(2)
        XCTAssertEqual(state, false)
    }

    func testGetAvailableDropsMissingObjectsAndKeepsTheRest() async throws {
        let agent = try FakeAgent(); defer { agent.stop() }
        agent["1.3.6.1.2.1.1.1.0"] = .integer(1)
        agent["1.3.6.1.2.1.1.3.0"] = .integer(3)
        let got = try await agent.transport.getAvailable([OID("1.3.6.1.2.1.1.1.0")!, OID("1.3.6.1.2.1.1.2.0")!, OID("1.3.6.1.2.1.1.3.0")!, OID("1.3.6.1.2.1.1.9.0")!])
        XCTAssertEqual(got.count, 2)
        XCTAssertEqual(got[OID("1.3.6.1.2.1.1.3.0")!], .integer(3))
    }
}
