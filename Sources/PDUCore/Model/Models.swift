import Foundation

public enum PDUVendor: String, Codable, CaseIterable, Sendable, Identifiable {
    case cyberPower, apc
    public var id: String { rawValue }
    public var displayName: String {
        switch self { case .cyberPower: return "CyberPower"; case .apc: return "APC" }
    }
}

public enum OutletAction: String, Sendable {
    case on, off
}

/// One outlet as read from a PDU. Values the PDU cannot measure are nil (not 0), so the screen can show "—".
public struct OutletReading: Identifiable, Equatable, Sendable {
    public var number: Int
    public var name: String
    public var bank: Int?
    public var isOn: Bool?
    public var amps: Double?
    public var watts: Double?
    public var id: Int { number }
    public init(number: Int, name: String, bank: Int? = nil, isOn: Bool? = nil, amps: Double? = nil, watts: Double? = nil) {
        self.number = number; self.name = name; self.bank = bank; self.isOn = isOn; self.amps = amps; self.watts = watts
    }
    /// The PDU's default names ("Outlet_7", "Outlet 7", "Outlet7", empty) mean nothing is assigned to the outlet.
    public var isAssigned: Bool { !ServerID.isDefaultOutletName(name) }
}

public struct BankReading: Identifiable, Equatable, Sendable {
    public var number: Int
    public var amps: Double
    public var watts: Double?
    public var id: Int { number }
    public init(number: Int, amps: Double, watts: Double? = nil) { self.number = number; self.amps = amps; self.watts = watts }
}

public struct PhaseReading: Identifiable, Equatable, Sendable {
    public var number: Int
    public var amps: Double
    public var watts: Double?
    public var volts: Double?
    public var id: Int { number }
    public init(number: Int, amps: Double, watts: Double? = nil, volts: Double? = nil) {
        self.number = number; self.amps = amps; self.watts = watts; self.volts = volts
    }
}

public struct PDUInfo: Equatable, Sendable {
    public var vendor: PDUVendor
    public var model: String?
    public var name: String?
    public var serial: String?
    public var firmware: String?
    public var outletCount: Int
    public var breakerCount: Int?
    public var orientation: String?
    public var lineVoltage: Double?
    public init(vendor: PDUVendor, model: String? = nil, name: String? = nil, serial: String? = nil, firmware: String? = nil,
                outletCount: Int = 0, breakerCount: Int? = nil, orientation: String? = nil, lineVoltage: Double? = nil) {
        self.vendor = vendor; self.model = model; self.name = name; self.serial = serial; self.firmware = firmware
        self.outletCount = outletCount; self.breakerCount = breakerCount; self.orientation = orientation; self.lineVoltage = lineVoltage
    }
}

/// Everything one poll of one PDU returns.
public struct PDUSnapshot: Equatable, Sendable {
    public var timestamp: Date
    public var info: PDUInfo
    public var outlets: [OutletReading]
    public var banks: [BankReading]
    public var phases: [PhaseReading]
    /// Whole-PDU totals. Taken from the device when it reports them, else summed from phases, banks or outlets.
    public var totalAmps: Double?
    public var totalWatts: Double?
    /// True when the PDU measures each outlet (not only the bank or the whole unit).
    public var hasOutletMetering: Bool { outlets.contains { $0.amps != nil || $0.watts != nil } }

    public init(timestamp: Date = Date(), info: PDUInfo, outlets: [OutletReading], banks: [BankReading] = [],
                phases: [PhaseReading] = [], totalAmps: Double? = nil, totalWatts: Double? = nil) {
        self.timestamp = timestamp; self.info = info; self.outlets = outlets; self.banks = banks; self.phases = phases
        self.totalAmps = totalAmps ?? PDUSnapshot.derivedAmps(phases: phases, banks: banks, outlets: outlets)
        self.totalWatts = totalWatts ?? PDUSnapshot.derivedWatts(phases: phases, outlets: outlets)
    }

    static func derivedAmps(phases: [PhaseReading], banks: [BankReading], outlets: [OutletReading]) -> Double? {
        // A three-phase PDU is limited per phase: the busiest phase is what is compared with the limit.
        if phases.count > 1 { return phases.map(\.amps).max() }
        if !phases.isEmpty { return phases.reduce(0) { $0 + $1.amps } }
        if !banks.isEmpty { return banks.reduce(0) { $0 + $1.amps } }
        let metered = outlets.compactMap(\.amps)
        return metered.isEmpty ? nil : metered.reduce(0, +)
    }
    static func derivedWatts(phases: [PhaseReading], outlets: [OutletReading]) -> Double? {
        let phaseWatts = phases.compactMap(\.watts)
        if !phaseWatts.isEmpty { return phaseWatts.reduce(0, +) }
        let metered = outlets.compactMap(\.watts)
        return metered.isEmpty ? nil : metered.reduce(0, +)
    }
}

/// Where a power-limit stands. Used for racks, PDUs and banks.
public enum LimitStatus: Int, Comparable, Sendable {
    case ok, warning, over, unknown
    public static func < (a: LimitStatus, b: LimitStatus) -> Bool { a.rawValue < b.rawValue }

    /// `warnFraction` is where the warning starts (0.8 = 80 % of the limit).
    public static func evaluate(amps: Double?, limit: Double?, warnFraction: Double = 0.8) -> LimitStatus {
        guard let amps, let limit, limit > 0 else { return .unknown }
        // The readings have one decimal; the tolerance keeps 19.2 A of 24 A at 80 % from falling below the line by rounding.
        if amps > limit + 1e-9 { return .over }
        if amps + 1e-9 >= limit * warnFraction { return .warning }
        return .ok
    }
}
