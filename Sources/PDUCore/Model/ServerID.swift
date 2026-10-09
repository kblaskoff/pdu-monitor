import Foundation

/// The servers are not known to the PDUs: the person names the outlets (for example "200U31" = rack 200, unit 31) and the
/// same name on two PDUs of a rack means one server with two power supplies.
public enum ServerID {
    /// "Outlet_7", "Outlet 7", "Outlet7", "OUTLET-07" and an empty name are the PDU's defaults.
    public static func isDefaultOutletName(_ name: String) -> Bool {
        let trimmed = name.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty { return true }
        let lower = trimmed.lowercased()
        guard lower.hasPrefix("outlet") else { return false }
        let rest = lower.dropFirst("outlet".count).drop { $0 == "_" || $0 == " " || $0 == "-" }
        return rest.isEmpty || rest.allSatisfy(\.isNumber)
    }

    /// The outlets that belong together are found by this key (case-insensitive, no spaces).
    public static func key(_ name: String) -> String {
        name.trimmingCharacters(in: .whitespacesAndNewlines).lowercased().replacingOccurrences(of: " ", with: "")
    }

    /// Network gear is named with an N in front of the rack-unit id: "N200U43", "N10FU30" (N, the rack, U, the unit).
    public static func kind(of name: String) -> DeviceKind {
        let upper = name.trimmingCharacters(in: .whitespaces).uppercased()
        guard upper.hasPrefix("N") else { return .server }
        let rest = upper.dropFirst()
        // the rack id starts with a digit and may have letters (200, 10F, 20E), then U and the unit number
        guard rest.first?.isNumber == true, let u = rest.lastIndex(of: "U") else { return .server }
        let rack = rest[rest.startIndex..<u]
        let unit = rest[rest.index(after: u)...].prefix { $0.isNumber }
        return (!rack.isEmpty && rack.allSatisfy { $0.isNumber || $0.isLetter } && !unit.isEmpty) ? .network : .server
    }
}

public enum DeviceKind: String, Codable, Sendable {
    case server, network
    public var displayName: String { self == .server ? "Server" : "Network" }
}
