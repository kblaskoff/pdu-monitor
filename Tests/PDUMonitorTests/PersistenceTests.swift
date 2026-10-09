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
