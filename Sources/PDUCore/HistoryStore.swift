import Foundation

/// One reading of one PDU, as it is kept for history (graphs come later).
public struct PowerSample: Codable, Equatable, Sendable {
    public var pduID: UUID
    public var rackID: UUID
    public var timestamp: Date
    public var amps: Double?
    public var watts: Double?
    public init(pduID: UUID, rackID: UUID, timestamp: Date, amps: Double?, watts: Double?) {
        self.pduID = pduID; self.rackID = rackID; self.timestamp = timestamp; self.amps = amps; self.watts = watts
    }
}

/// Where the history goes. The application calls `record` after every successful poll; today nothing is stored
/// (`NullHistoryStore`), a database implementation will be added without changing the callers.
public protocol HistoryStore: Sendable {
    func record(_ samples: [PowerSample]) async
    func samples(rack: UUID, since: Date) async -> [PowerSample]
}

public struct NullHistoryStore: HistoryStore {
    public init() {}
    public func record(_ samples: [PowerSample]) async {}
    public func samples(rack: UUID, since: Date) async -> [PowerSample] { [] }
}
