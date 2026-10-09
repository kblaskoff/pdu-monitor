import Foundation

/// One PDU, whatever the make: read it, switch an outlet, read an outlet's state.
public protocol PDUDriver: Sendable {
    var vendor: PDUVendor { get }
    func poll() async throws -> PDUSnapshot
    func setOutlet(_ number: Int, on: Bool) async throws
    /// nil when the PDU cannot tell.
    func outletState(_ number: Int) async throws -> Bool?
}

public enum PDUDriverFactory {
    public static func make(vendor: PDUVendor, transport: SNMPTransport) -> PDUDriver {
        switch vendor {
        case .cyberPower: return CyberPowerDriver(transport: transport)
        case .apc: return APCDriver(transport: transport)
        }
    }
}

enum DriverError: Error, LocalizedError {
    case notRecognized(String)
    var errorDescription: String? {
        switch self { case .notRecognized(let vendor): return "The device does not answer like a \(vendor) PDU (wrong vendor, community or SNMP disabled?)" }
    }
}

extension Dictionary where Key == OID, Value == SNMPValue {
    func int(_ oid: OID) -> Int? { self[oid]?.intValue }
    func string(_ oid: OID) -> String? {
        guard let text = self[oid]?.stringValue, !text.isEmpty else { return nil }
        return text
    }
    /// Tenths of a unit (amps, volts) as a number; negative means "not supported" for some APC objects.
    func tenths(_ oid: OID) -> Double? {
        guard let v = int(oid), v >= 0 else { return nil }
        return Double(v) / 10
    }
}

/// Puts the bank's wattage together from the outlets (the PDUs do not report it), when the outlets are metered.
func banksWithWatts(_ banks: [BankReading], outlets: [OutletReading]) -> [BankReading] {
    banks.map { bank in
        var bank = bank
        let members = outlets.filter { $0.bank == bank.number }.compactMap(\.watts)
        if bank.watts == nil, !members.isEmpty { bank.watts = members.reduce(0, +) }
        return bank
    }
}
