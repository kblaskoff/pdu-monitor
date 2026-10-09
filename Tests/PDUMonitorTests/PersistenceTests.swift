import XCTest
import PDUCore
@testable import PDUMonitor

/// Adding and editing racks and PDUs must reach the settings file (and the communities must stay out of it).
final class PersistenceTests: XCTestCase {
    @MainActor func testAddedAndEditedDevicesAreSaved() throws {
        let original = try? Data(contentsOf: ConfigStore.url)
        defer { if let original { try? original.write(to: ConfigStore.url) } else { try? FileManager.default.removeItem(at: ConfigStore.url) } }
        try? FileManager.default.removeItem(at: ConfigStore.url)
        let model = AppModel()
        model.settings.demoMode = false
        let rack = RackConfig(name: "200")
        model.addRack(rack)
        var device = DeviceConfig(name: "P200A", rackID: rack.id, vendor: .apc, host: "192.0.2.10", readCommunity: "secret-read")
        model.addDevice(device)
        var file = ConfigStore.load()
        XCTAssertEqual(file.racks.map(\.name), ["200"])
        XCTAssertEqual(file.devices.map(\.name), ["P200A"])
        XCTAssertEqual(file.devices.first?.vendor, .apc)
        let raw = try String(contentsOf: ConfigStore.url, encoding: .utf8)
        XCTAssertFalse(raw.contains("secret-read"))
        device.maxAmps = 16
        device.host = "192.0.2.11"
        model.updateDevice(device)
        file = ConfigStore.load()
        XCTAssertEqual(file.devices.first?.host, "192.0.2.11")
        XCTAssertEqual(file.devices.first?.maxAmps, 16)
        model.deleteDevice(device.id)
        XCTAssertTrue(ConfigStore.load().devices.isEmpty)
        Secrets.remove(device.id)
    }
}

final class BackupTests: XCTestCase {
    @MainActor func testExportAndImportRoundTrip() throws {
        let original = try? Data(contentsOf: ConfigStore.url)
        defer { if let original { try? original.write(to: ConfigStore.url) } else { try? FileManager.default.removeItem(at: ConfigStore.url) } }
        let model = AppModel()
        model.settings.demoMode = false
        let rack = RackConfig(name: "10F", maxAmps: 20)
        model.addRack(rack)
        let device = DeviceConfig(name: "P10FA", rackID: rack.id, vendor: .cyberPower, host: "192.0.2.20", readCommunity: "r-secret", writeCommunity: "w-secret")
        model.addDevice(device)
        model.labels.set("web-01", rack: rack.id, id: "10FU1")

        let plain = try model.backupData(includeCommunities: false)
        XCTAssertFalse(String(decoding: plain, as: UTF8.self).contains("r-secret"))
        let withSecrets = try model.backupData(includeCommunities: true)
        XCTAssertTrue(String(decoding: withSecrets, as: UTF8.self).contains("w-secret"))

        // wipe, then restore from the file that has the communities
        model.deleteDevice(device.id)
        model.deleteRack(rack.id)
        XCTAssertTrue(model.racks.isEmpty)
        let summary = try model.restore(from: withSecrets)
        XCTAssertEqual(summary.racks, 1)
        XCTAssertEqual(summary.devices, 1)
        XCTAssertTrue(summary.withoutCommunity.isEmpty)
        XCTAssertEqual(model.racks.first?.maxAmps, 20)
        XCTAssertEqual(model.devices.first?.readCommunity, "r-secret")
        XCTAssertEqual(model.devices.first?.writeCommunity, "w-secret")
        XCTAssertEqual(model.labels.label(rack: rack.id, id: "10FU1"), "web-01")

        // a file without communities keeps the ones the application already has for the same PDU
        _ = try model.restore(from: plain)
        XCTAssertEqual(model.devices.first?.readCommunity, "r-secret")
        // and reports the PDU when it has none
        model.deleteDevice(device.id)
        let report = try model.restore(from: plain)
        XCTAssertEqual(report.withoutCommunity, ["P10FA"])

        XCTAssertThrowsError(try model.restore(from: Data("{\"hello\":1}".utf8)))
        model.deleteDevice(device.id)
        model.deleteRack(rack.id)
        Secrets.remove(device.id)
    }
}
