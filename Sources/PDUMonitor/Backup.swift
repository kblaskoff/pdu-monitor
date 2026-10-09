import SwiftUI
import AppKit
import UniformTypeIdentifiers
import PDUCore

/// The settings as one JSON file: racks, PDUs, server names and the application settings. The SNMP communities are only in the
/// file when the person asks for them (the file is then as private as a password list).
struct BackupFile: Codable {
    struct Communities: Codable { var read: String; var write: String }
    static let formatName = "pdu-monitor-settings"
    var format = BackupFile.formatName
    var version = 1
    var exported = Date()
    var racks: [RackConfig]
    var devices: [DeviceConfig]
    var labels: ServerLabels
    var settings: AppSettings
    var communities: [String: Communities]?
}

struct RestoreSummary {
    var racks: Int
    var devices: Int
    /// PDUs that came without a community: it has to be typed in Settings → PDUs.
    var withoutCommunity: [String]
}

enum BackupError: LocalizedError {
    case notABackup
    case newerVersion
    var errorDescription: String? {
        switch self {
        case .notABackup: return "This file is not a PDU Monitor settings file."
        case .newerVersion: return "This file was made by a newer version of PDU Monitor. Update the application first."
        }
    }
}

extension AppModel {
    func backupData(includeCommunities: Bool) throws -> Data {
        // In demo mode the sample racks are not the person's settings: the real ones are exported.
        let realRacks = stashedRacks ?? racks
        let realDevices = stashedDevices ?? devices
        var file = BackupFile(racks: realRacks, devices: realDevices, labels: labels, settings: settings, communities: nil)
        file.settings.demoMode = false
        if includeCommunities {
            file.communities = Dictionary(uniqueKeysWithValues: realDevices.map { ($0.id.uuidString, .init(read: $0.readCommunity, write: $0.writeCommunity)) })
        }
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        encoder.dateEncodingStrategy = .iso8601
        return try encoder.encode(file)
    }

    /// Replaces the racks, PDUs, server names and settings with the ones of the file.
    @discardableResult
    func restore(from data: Data) throws -> RestoreSummary {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        guard let file = try? decoder.decode(BackupFile.self, from: data), file.format == BackupFile.formatName else { throw BackupError.notABackup }
        guard file.version <= 1 else { throw BackupError.newerVersion }
        if settings.demoMode { settings.demoMode = false }
        let oldCommunities = Dictionary(uniqueKeysWithValues: devices.map { ($0.id, ($0.readCommunity, $0.writeCommunity)) })
        for old in devices where !file.devices.contains(where: { $0.id == old.id }) { Secrets.remove(old.id) }
        var restored = file.devices
        var missing: [String] = []
        for i in restored.indices {
            let id = restored[i].id
            if let given = file.communities?[id.uuidString] {
                restored[i].readCommunity = given.read; restored[i].writeCommunity = given.write
            } else if let kept = oldCommunities[id] {
                restored[i].readCommunity = kept.0; restored[i].writeCommunity = kept.1
            }
            if restored[i].readCommunity.isEmpty { missing.append(restored[i].name) }
            Secrets.save(restored[i])
        }
        racks = file.racks
        devices = restored
        labels = file.labels
        var newSettings = file.settings
        newSettings.demoMode = false
        settings = newSettings
        clearStates()
        route = .overview
        rebuildDriversAfterRestore()
        save()
        Task { await poll() }
        return RestoreSummary(racks: file.racks.count, devices: restored.count, withoutCommunity: missing)
    }
}

/// The save and open panels of the backup.
@MainActor
enum BackupUI {
    static func export(_ model: AppModel) {
        let panel = NSSavePanel()
        panel.allowedContentTypes = [.json]
        let day = ISO8601DateFormatter().string(from: Date()).prefix(10)
        panel.nameFieldStringValue = "PDU Monitor settings \(day).json"
        panel.message = "Racks, PDUs, server names and settings."
        let box = NSButton(checkboxWithTitle: "Include the SNMP communities (then keep the file private)", target: nil, action: nil)
        box.state = .off
        box.sizeToFit()
        let holder = NSView(frame: NSRect(x: 0, y: 0, width: box.frame.width + 32, height: box.frame.height + 16))
        box.frame.origin = NSPoint(x: 16, y: 8)
        holder.addSubview(box)
        panel.accessoryView = holder
        guard panel.runModal() == .OK, let url = panel.url else { return }
        do {
            try model.backupData(includeCommunities: box.state == .on).write(to: url, options: .atomic)
        } catch {
            show("Could not save the file", error.localizedDescription)
        }
    }

    static func importSettings(_ model: AppModel) {
        let panel = NSOpenPanel()
        panel.allowedContentTypes = [.json]
        panel.allowsMultipleSelection = false
        panel.message = "Choose a PDU Monitor settings file."
        guard panel.runModal() == .OK, let url = panel.url else { return }
        let confirm = NSAlert()
        confirm.messageText = "Replace the current settings?"
        confirm.informativeText = "All racks, PDUs, server names and settings are replaced by the ones in the file."
        confirm.addButton(withTitle: "Replace")
        confirm.addButton(withTitle: "Cancel")
        guard confirm.runModal() == .alertFirstButtonReturn else { return }
        do {
            let summary = try model.restore(from: try Data(contentsOf: url))
            var text = "\(summary.racks) rack(s) and \(summary.devices) PDU(s) loaded."
            if !summary.withoutCommunity.isEmpty {
                text += "\n\nThe file has no SNMP communities. Type them in Settings → PDUs for: " + summary.withoutCommunity.joined(separator: ", ") + "."
            }
            show("Settings loaded", text)
        } catch {
            show("Could not load the file", error.localizedDescription)
        }
    }

    private static func show(_ title: String, _ text: String) {
        let alert = NSAlert()
        alert.messageText = title
        alert.informativeText = text
        alert.runModal()
    }
}
