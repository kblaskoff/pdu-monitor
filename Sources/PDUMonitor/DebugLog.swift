import Foundation
import AppKit
import PDUCore

/// One text file with what the application did on the network: ~/Library/Logs/PDU Monitor/debug.log
/// (every SNMP request and the answer's status, every failed read). Settings → General → "Show debug log".
/// Communities and passwords are never written. The file is cut when it grows past 1 MB.
enum DebugLog {
    static let url: URL = {
        let base = FileManager.default.urls(for: .libraryDirectory, in: .userDomainMask)[0]
        return base.appendingPathComponent("Logs/PDU Monitor/debug.log")
    }()
    private static let queue = DispatchQueue(label: "pdumonitor.debuglog")
    private static let formatter: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter(); f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]; return f
    }()

    static func start() {
        SNMPDebug.log = { write($0) }
        write("--- PDU Monitor \(AppVersion.display) started")
    }

    static func write(_ line: String) {
        queue.async {
            let text = "\(formatter.string(from: Date())) \(line)\n"
            let fm = FileManager.default
            try? fm.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
            if let size = (try? fm.attributesOfItem(atPath: url.path))?[.size] as? Int, size > 1_000_000 { try? fm.removeItem(at: url) }
            if !fm.fileExists(atPath: url.path) { fm.createFile(atPath: url.path, contents: nil) }
            guard let handle = try? FileHandle(forWritingTo: url) else { return }
            defer { try? handle.close() }
            _ = try? handle.seekToEnd()
            try? handle.write(contentsOf: Data(text.utf8))
        }
    }

    static func reveal() {
        write("--- log opened")
        queue.sync {}
        NSWorkspace.shared.activateFileViewerSelecting([url])
    }
}
