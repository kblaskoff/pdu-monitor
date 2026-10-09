import Foundation

/// CyberPower Switched Metered-by-Outlet ePDU (PDU81xxx and the other ePDU2 models): CPS-MIB, ePDU2 = 1.3.6.1.4.1.3808.1.1.6
public struct CyberPowerDriver: PDUDriver {
    public let transport: SNMPTransport
    public var vendor: PDUVendor { .cyberPower }
    public init(transport: SNMPTransport) { self.transport = transport }

    static let root = OID([1, 3, 6, 1, 4, 1, 3808, 1, 1, 6])
    static func oid(_ path: [UInt32], _ index: Int) -> OID { OID(root.components + path + [UInt32(index)]) }

    // Single-row tables (index 1 is the PDU itself)
    static let model = [2, 2, 1, 9] as [UInt32], name = [2, 2, 1, 3] as [UInt32], serial = [2, 2, 1, 10] as [UInt32], firmware = [2, 2, 1, 7] as [UInt32]
    static let numOutlets = [3, 3, 1, 5] as [UInt32], numPhases = [3, 3, 1, 8] as [UInt32], numBreakers = [3, 3, 1, 9] as [UInt32]
    static let orientation = [3, 3, 1, 11] as [UInt32]
    static let deviceCurrent = [3, 4, 1, 5] as [UInt32]       // tenths of amps
    static let devicePower = [3, 4, 1, 18] as [UInt32]        // watts
    // Per phase
    static let phaseLoad = [4, 4, 1, 5] as [UInt32], phaseVolts = [4, 4, 1, 6] as [UInt32], phasePower = [4, 4, 1, 7] as [UInt32]
    static let phaseLineToLine = [4, 4, 1, 13] as [UInt32]
    // Per bank (breaker)
    static let bankLoad = [5, 4, 1, 5] as [UInt32]
    // Per outlet
    static let outletName = [6, 1, 4, 1, 4] as [UInt32], outletState = [6, 1, 4, 1, 5] as [UInt32]
    static let outletBank = [6, 1, 3, 1, 6] as [UInt32]
    static let meteredLoad = [6, 2, 4, 1, 6] as [UInt32], meteredPower = [6, 2, 4, 1, 7] as [UInt32]
    static let outletCommand = [6, 1, 5, 1, 5] as [UInt32]    // 1 on, 2 off (read: the same)

    public func poll() async throws -> PDUSnapshot {
        let head = try await transport.getAvailable([
            Self.oid(Self.model, 1), Self.oid(Self.name, 1), Self.oid(Self.serial, 1), Self.oid(Self.firmware, 1),
            Self.oid(Self.numOutlets, 1), Self.oid(Self.numPhases, 1), Self.oid(Self.numBreakers, 1), Self.oid(Self.orientation, 1),
            Self.oid(Self.deviceCurrent, 1), Self.oid(Self.devicePower, 1)
        ])
        guard let outletCount = head.int(Self.oid(Self.numOutlets, 1)), outletCount > 0 else {
            throw DriverError.notRecognized("CyberPower")
        }
        let phaseCount = max(1, head.int(Self.oid(Self.numPhases, 1)) ?? 1)
        let bankCount = head.int(Self.oid(Self.numBreakers, 1)) ?? 0

        var wanted: [OID] = []
        for p in 1...phaseCount {
            wanted += [Self.oid(Self.phaseLoad, p), Self.oid(Self.phaseVolts, p), Self.oid(Self.phasePower, p), Self.oid(Self.phaseLineToLine, p)]
        }
        if bankCount > 0 { for b in 1...bankCount { wanted.append(Self.oid(Self.bankLoad, b)) } }
        for o in 1...outletCount {
            wanted += [Self.oid(Self.outletName, o), Self.oid(Self.outletState, o), Self.oid(Self.outletBank, o),
                       Self.oid(Self.meteredLoad, o), Self.oid(Self.meteredPower, o)]
        }
        let values = try await transport.getAvailable(wanted)

        let phases: [PhaseReading] = (1...phaseCount).compactMap { p in
            guard let amps = values.tenths(Self.oid(Self.phaseLoad, p)) else { return nil }
            return PhaseReading(number: p, amps: amps, watts: values.int(Self.oid(Self.phasePower, p)).map(Double.init),
                                volts: values.tenths(Self.oid(Self.phaseVolts, p)))
        }
        let outlets: [OutletReading] = (1...outletCount).map { o in
            OutletReading(number: o, name: values.string(Self.oid(Self.outletName, o)) ?? "",
                          bank: values.int(Self.oid(Self.outletBank, o)),
                          isOn: values.int(Self.oid(Self.outletState, o)).map { $0 == 1 },
                          amps: values.tenths(Self.oid(Self.meteredLoad, o)),
                          watts: values.int(Self.oid(Self.meteredPower, o)).map(Double.init))
        }
        var banks: [BankReading] = []
        if bankCount > 0 {
            banks = (1...bankCount).compactMap { b in values.tenths(Self.oid(Self.bankLoad, b)).map { BankReading(number: b, amps: $0) } }
        }
        let lineVoltage = values.tenths(Self.oid(Self.phaseLineToLine, 1)) ?? phases.first?.volts
        let info = PDUInfo(vendor: .cyberPower, model: head.string(Self.oid(Self.model, 1)), name: head.string(Self.oid(Self.name, 1)),
                           serial: head.string(Self.oid(Self.serial, 1)), firmware: head.string(Self.oid(Self.firmware, 1)),
                           outletCount: outletCount, breakerCount: bankCount > 0 ? bankCount : nil,
                           orientation: head.int(Self.oid(Self.orientation, 1)).map { $0 == 2 ? "Vertical" : "Horizontal" },
                           lineVoltage: lineVoltage)
        return PDUSnapshot(info: info, outlets: outlets, banks: banksWithWatts(banks, outlets: outlets), phases: phases,
                           totalAmps: head.tenths(Self.oid(Self.deviceCurrent, 1)),
                           totalWatts: head.int(Self.oid(Self.devicePower, 1)).map(Double.init))
    }

    public func setOutlet(_ number: Int, on: Bool) async throws {
        try await transport.set([VarBind(Self.oid(Self.outletCommand, number), .integer(on ? 1 : 2))])
    }

    public func outletState(_ number: Int) async throws -> Bool? {
        let values = try await transport.getAvailable([Self.oid(Self.outletState, number)])
        return values.int(Self.oid(Self.outletState, number)).map { $0 == 1 }
    }
}
