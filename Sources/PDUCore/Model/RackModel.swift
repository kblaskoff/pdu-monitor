import Foundation

/// A PDU of a rack together with its last reading.
public struct PDUState: Identifiable, Sendable {
    public var config: DeviceConfig
    public var snapshot: PDUSnapshot?
    public var error: String?
    public var id: UUID { config.id }
    public init(config: DeviceConfig, snapshot: PDUSnapshot? = nil, error: String? = nil) {
        self.config = config; self.snapshot = snapshot; self.error = error
    }
    public var amps: Double? { snapshot?.totalAmps }
    public var watts: Double? { snapshot?.totalWatts }
    public func status(warnFraction: Double = 0.8) -> LimitStatus { LimitStatus.evaluate(amps: amps, limit: config.maxAmps, warnFraction: warnFraction) }
}

/// A reference to one physical port: which PDU, which outlet.
public struct OutletTarget: Hashable, Sendable {
    public var pduID: UUID
    public var outlet: Int
    public init(pduID: UUID, outlet: Int) { self.pduID = pduID; self.outlet = outlet }
}

public enum ServerPower: Sendable { case on, off, mixed, unknown }

/// One server (or switch) of a rack: the outlets named the same on the rack's PDUs, put together.
public struct ServerEntry: Identifiable, Sendable {
    public struct Port: Identifiable, Sendable {
        public var target: OutletTarget
        public var pduName: String
        public var reading: OutletReading
        public var id: OutletTarget { target }
    }
    public var key: String
    public var name: String
    public var kind: DeviceKind
    public var ports: [Port]
    public var id: String { key }

    /// nil when no port of the server is metered (some APC models).
    public var watts: Double? { let v = ports.compactMap(\.reading.watts); return v.isEmpty ? nil : v.reduce(0, +) }
    public var amps: Double? { let v = ports.compactMap(\.reading.amps); return v.isEmpty ? nil : v.reduce(0, +) }
    public var power: ServerPower {
        let states = ports.map(\.reading.isOn)
        if states.contains(nil) { return states.allSatisfy { $0 == nil } ? .unknown : .mixed }
        if states.allSatisfy({ $0 == true }) { return .on }
        if states.allSatisfy({ $0 == false }) { return .off }
        return .mixed
    }
    public var targets: [OutletTarget] { ports.map(\.target) }
}

public enum RackAggregator {
    /// Servers of a rack, from the last readings of its PDUs. Outlets with the PDU's default name are not servers.
    public static func servers(of pdus: [PDUState]) -> [ServerEntry] {
        var order: [String] = []
        var map: [String: ServerEntry] = [:]
        for pdu in pdus {
            guard let snapshot = pdu.snapshot else { continue }
            for outlet in snapshot.outlets where outlet.isAssigned {
                let key = ServerID.key(outlet.name)
                let port = ServerEntry.Port(target: OutletTarget(pduID: pdu.id, outlet: outlet.number), pduName: pdu.config.name, reading: outlet)
                if map[key] == nil {
                    let name = outlet.name.trimmingCharacters(in: .whitespacesAndNewlines)
                    map[key] = ServerEntry(key: key, name: name, kind: ServerID.kind(of: name), ports: [port]); order.append(key)
                } else {
                    map[key]!.ports.append(port)
                }
            }
        }
        return order.compactMap { map[$0] }.sorted { naturalLess($0.name, $1.name) }
    }

    /// "200U9" before "200U31".
    static func naturalLess(_ a: String, _ b: String) -> Bool {
        a.compare(b, options: [.numeric, .caseInsensitive]) == .orderedAscending
    }
}

public struct RackSummary: Sendable {
    public var amps: Double?
    public var watts: Double?
    public var limit: Double
    public var status: LimitStatus
    public var worstPDU: LimitStatus
    public var pduCount: Int
    public var offlineCount: Int
    /// Share of the limit that is used (0…); nil when no PDU of the rack was read yet.
    public var fraction: Double? { amps.map { limit > 0 ? $0 / limit : 0 } }

    /// The rack is as bad as its worst part: the rack limit and each PDU's own limit both count.
    public var attention: LimitStatus { max(status, worstPDU == .unknown ? .ok : worstPDU) }
}

public enum RackSummarizer {
    public static func summary(rack: RackConfig, pdus: [PDUState], warnFraction: Double = 0.8) -> RackSummary {
        let read = pdus.compactMap(\.amps)
        let amps: Double? = read.isEmpty ? nil : read.reduce(0, +)
        let watts: Double? = { let w = pdus.compactMap(\.watts); return w.isEmpty ? nil : w.reduce(0, +) }()
        let worst = pdus.map { $0.status(warnFraction: warnFraction) }.filter { $0 != .unknown }.max() ?? .unknown
        return RackSummary(amps: amps, watts: watts, limit: rack.maxAmps,
                           status: LimitStatus.evaluate(amps: amps, limit: rack.maxAmps, warnFraction: warnFraction), worstPDU: worst,
                           pduCount: pdus.count, offlineCount: pdus.filter { $0.snapshot == nil || $0.error != nil }.count)
    }
}
