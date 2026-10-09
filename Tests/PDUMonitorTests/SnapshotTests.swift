import AppKit
import SwiftUI
import XCTest
import PDUCore
@testable import PDUMonitor

/// Draws the real window with the demo racks into PNG files, so the screens can be reviewed without a Mac at hand.
/// Runs only when PDUMONITOR_SNAPSHOTS names an output folder (the CI workflow sets it).
final class SnapshotTests: XCTestCase {
    enum SnapshotError: Error { case noImage }

    @MainActor private func render<V: View>(_ view: V, size: CGSize, dark: Bool, name: String, in folder: URL) throws {
        let host = NSHostingView(rootView: view.background(Color(nsColor: .windowBackgroundColor)))
        host.frame = NSRect(origin: .zero, size: size)
        let window = NSWindow(contentRect: host.frame, styleMask: [.titled, .resizable], backing: .buffered, defer: false)
        window.isReleasedWhenClosed = false
        window.appearance = NSAppearance(named: dark ? .darkAqua : .aqua)
        window.contentView = host
        window.setFrame(NSRect(origin: .zero, size: size), display: true)
        window.makeKeyAndOrderFront(nil)
        host.layoutSubtreeIfNeeded()
        RunLoop.current.run(until: Date().addingTimeInterval(0.8))
        host.layoutSubtreeIfNeeded()
        guard let rep = host.bitmapImageRepForCachingDisplay(in: host.bounds) else { throw SnapshotError.noImage }
        host.cacheDisplay(in: host.bounds, to: rep)
        guard let png = rep.representation(using: .png, properties: [:]) else { throw SnapshotError.noImage }
        try png.write(to: folder.appendingPathComponent("\(name)-\(dark ? "dark" : "light").png"))
        window.close()
    }

    @MainActor func testRenderSnapshots() async throws {
        guard let path = ProcessInfo.processInfo.environment["PDUMONITOR_SNAPSHOTS"], !path.isEmpty else {
            throw XCTSkip("Set PDUMONITOR_SNAPSHOTS to a folder to write screenshots.")
        }
        _ = NSApplication.shared
        let folder = URL(fileURLWithPath: path, isDirectory: true)
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        let model = AppModel()
        model.settings.demoMode = true
        await model.poll()
        let size = CGSize(width: 1000, height: 640)
        for dark in [false, true] {
            model.route = .overview
            model.settings.overviewAsList = false
            try render(ContentView().environmentObject(model), size: size, dark: dark, name: "overview", in: folder)
            if !dark {
                model.settings.overviewAsList = true
                try render(ContentView().environmentObject(model), size: size, dark: dark, name: "overview-list", in: folder)
                model.settings.overviewAsList = false
            }
            if let rack = model.racks.first(where: { $0.name == "200" }) {
                model.route = .rack(rack.id)
                try render(ContentView().environmentObject(model), size: size, dark: dark, name: "rack-200", in: folder)
            }
            if let rack = model.racks.first(where: { $0.name == "20F" }) {
                model.route = .rack(rack.id)
                try render(ContentView().environmentObject(model), size: size, dark: dark, name: "rack-20F-over", in: folder)
            }
            if let device = model.devices.first(where: { $0.name == "P200B" }) {
                model.route = .pdu(device.id)
                try render(ContentView().environmentObject(model), size: size, dark: dark, name: "pdu-P200B", in: folder)
            }
            if let device = model.devices.first(where: { $0.name == "P200A" }) {
                model.route = .pdu(device.id)
                try render(ContentView().environmentObject(model), size: size, dark: dark, name: "pdu-P200A-unmetered", in: folder)
            }
        }
        model.route = .overview
        try render(SettingsView().environmentObject(model), size: CGSize(width: 640, height: 460), dark: false, name: "settings", in: folder)
        if let device = model.devices.first {
            try render(DeviceEditor(device: device, isNew: false, onClose: {}).environmentObject(model).padding(20),
                       size: CGSize(width: 700, height: 580), dark: false, name: "device-editor", in: folder)
        }
        model.settings.demoMode = false
    }
}
