import Foundation
import Security
import PDUCore

struct AppSettings: Codable, Equatable {
    var pollInterval: Double = 10
    /// Seconds the ports stay off during a restart.
    var restartDelay: Double = 8
    /// Where the yellow warning starts, in percent of a limit.
    var warnPercent: Double = 80
    var notifyWhenOver = true
    var demoMode = false
    var sortByLoad = false
    var overviewAsList = false

    var warnFraction: Double { warnPercent / 100 }

    init() {}
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let d = AppSettings()
        pollInterval = try c.decodeIfPresent(Double.self, forKey: .pollInterval) ?? d.pollInterval
        restartDelay = try c.decodeIfPresent(Double.self, forKey: .restartDelay) ?? d.restartDelay
        warnPercent = try c.decodeIfPresent(Double.self, forKey: .warnPercent) ?? d.warnPercent
        notifyWhenOver = try c.decodeIfPresent(Bool.self, forKey: .notifyWhenOver) ?? d.notifyWhenOver
        demoMode = try c.decodeIfPresent(Bool.self, forKey: .demoMode) ?? d.demoMode
        sortByLoad = try c.decodeIfPresent(Bool.self, forKey: .sortByLoad) ?? d.sortByLoad
        overviewAsList = try c.decodeIfPresent(Bool.self, forKey: .overviewAsList) ?? d.overviewAsList
    }
}

struct ConfigFile: Codable {
    var version = 1
    var racks: [RackConfig] = []
    var devices: [DeviceConfig] = []
    var labels = ServerLabels()
    var settings = AppSettings()
}

/// The settings file: ~/Library/Application Support/PDU Monitor/config.json (no passwords or communities in it).
enum ConfigStore {
    static var url: URL {
        let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        return base.appendingPathComponent("PDU Monitor", isDirectory: true).appendingPathComponent("config.json")
    }

    static func load() -> ConfigFile {
        guard let data = try? Data(contentsOf: url), let file = try? JSONDecoder().decode(ConfigFile.self, from: data) else { return ConfigFile() }
        return file
    }

    static func save(_ file: ConfigFile) {
        do {
            try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
            let encoder = JSONEncoder()
            encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
            try encoder.encode(file).write(to: url, options: .atomic)
        } catch {
            NSLog("PDU Monitor: could not save the settings: \(error.localizedDescription)")
        }
    }
}

/// The SNMP communities are in the macOS Keychain (one item per device and kind).
enum Secrets {
    static let service = "com.pdumonitor.app.snmp"

    private static func query(_ device: UUID, _ kind: String) -> [String: Any] {
        [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: "\(device.uuidString).\(kind)"]
    }

    static func read(_ device: UUID, _ kind: String) -> String {
        var q = query(device, kind)
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var result: CFTypeRef?
        guard SecItemCopyMatching(q as CFDictionary, &result) == errSecSuccess, let data = result as? Data else { return "" }
        return String(decoding: data, as: UTF8.self)
    }

    static func write(_ value: String, _ device: UUID, _ kind: String) {
        if value.isEmpty { delete(device, kind); return }
        let q = query(device, kind)
        let data = Data(value.utf8)
        if SecItemUpdate(q as CFDictionary, [kSecValueData as String: data] as CFDictionary) == errSecSuccess { return }
        var add = q
        add[kSecValueData as String] = data
        add[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
        SecItemAdd(add as CFDictionary, nil)
    }

    static func delete(_ device: UUID, _ kind: String) { SecItemDelete(query(device, kind) as CFDictionary) }

    static func load(into device: inout DeviceConfig) {
        device.readCommunity = read(device.id, "read")
        device.writeCommunity = read(device.id, "write")
    }
    static func save(_ device: DeviceConfig) {
        write(device.readCommunity, device.id, "read")
        write(device.writeCommunity, device.id, "write")
    }
    static func remove(_ device: UUID) { delete(device, "read"); delete(device, "write") }
}
