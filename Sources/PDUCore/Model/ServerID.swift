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

    /// Network gear is named with an N in front of the rack-unit id: "N200U43". Everything else is a server.
    public static func kind(of name: String) -> DeviceKind {
        let upper = name.trimmingCharacters(in: .whitespaces).uppercased()
        guard upper.hasPrefix("N") else { return .server }
        let rest = upper.dropFirst()
        // "N" + digits + "U" + digits, e.g. N200U43
        let digits = rest.prefix { $0.isNumber }
        guard !digits.isEmpty else { return .server }
        let afterDigits = rest.dropFirst(digits.count)
        guard afterDigits.first == "U", afterDigits.dropFirst().first?.isNumber == true else { return .server }
        return .network
    }
}

public enum DeviceKind: String, Codable, Sendable {
    case server, network
    public var displayName: String { self == .server ? "Server" : "Network" }
}
