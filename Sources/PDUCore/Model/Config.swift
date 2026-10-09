import Foundation

public struct RackConfig: Codable, Identifiable, Equatable, Sendable {
    public var id: UUID
    public var name: String
    /// The limit for the whole rack (all its PDUs together), in amps.
    public var maxAmps: Double
    public init(id: UUID = UUID(), name: String, maxAmps: Double = 24) { self.id = id; self.name = name; self.maxAmps = maxAmps }
}

public struct DeviceConfig: Codable, Identifiable, Equatable, Sendable {
    public var id: UUID
    public var name: String
    public var rackID: UUID
    public var vendor: PDUVendor
    public var host: String
    public var port: Int
    /// The limit for this PDU alone, in amps.
    public var maxAmps: Double
    /// Optional limit for one bank (breaker) of this PDU.
    public var bankMaxAmps: Double?
    /// The SNMPv1 communities are kept in the Keychain, never in the settings file (see `CodingKeys`).
    public var readCommunity: String
    public var writeCommunity: String

    public init(id: UUID = UUID(), name: String, rackID: UUID, vendor: PDUVendor, host: String, port: Int = 161, maxAmps: Double = 24,
                bankMaxAmps: Double? = nil, readCommunity: String = "public", writeCommunity: String = "") {
        self.id = id; self.name = name; self.rackID = rackID; self.vendor = vendor; self.host = host; self.port = port
        self.maxAmps = maxAmps; self.bankMaxAmps = bankMaxAmps; self.readCommunity = readCommunity; self.writeCommunity = writeCommunity
    }

    enum CodingKeys: String, CodingKey { case id, name, rackID, vendor, host, port, maxAmps, bankMaxAmps }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(UUID.self, forKey: .id)
        name = try c.decode(String.self, forKey: .name)
        rackID = try c.decode(UUID.self, forKey: .rackID)
        vendor = try c.decode(PDUVendor.self, forKey: .vendor)
        host = try c.decode(String.self, forKey: .host)
        port = try c.decodeIfPresent(Int.self, forKey: .port) ?? 161
        maxAmps = try c.decodeIfPresent(Double.self, forKey: .maxAmps) ?? 24
        bankMaxAmps = try c.decodeIfPresent(Double.self, forKey: .bankMaxAmps)
        readCommunity = ""; writeCommunity = ""
    }
}

/// What the person typed for servers: a nicer name for an outlet id such as "200U31".
public struct ServerLabels: Codable, Equatable, Sendable {
    /// rack id (uuid string) -> (server key -> label). Plain string keys keep the JSON file readable.
    public var labels: [String: [String: String]] = [:]
    public init() {}
    public func label(rack: UUID, id: String) -> String? {
        let value = labels[rack.uuidString]?[ServerID.key(id)]
        return (value?.isEmpty == false) ? value : nil
    }
    public mutating func set(_ label: String, rack: UUID, id: String) {
        var rackLabels = labels[rack.uuidString] ?? [:]
        let trimmed = label.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty { rackLabels.removeValue(forKey: ServerID.key(id)) } else { rackLabels[ServerID.key(id)] = trimmed }
        labels[rack.uuidString] = rackLabels
    }
}
