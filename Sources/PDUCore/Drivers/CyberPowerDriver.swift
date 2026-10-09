import Foundation

/// CyberPower PDUs. Two generations are understood, the driver asks the device which one it has and remembers it:
///  * ePDU2 (CPS-MIB 1.3.6.1.4.1.3808.1.1.6): PDU81xxx, PDU41xxx, PDU31xxx, PDU23xxx and the other current models: switched and/or metered
///    by outlet, monitored (names only), metered by unit/bank, 1 or 3 phases.
///  * ePDU (1.3.6.1.4.1.3808.1.1.3): the older "SW" models (PDU15SW8FNET, PDU20SWHVIEC8FNET, PDU30SWT10ATNET ...).
public final class CyberPowerDriver: PDUDriver, @unchecked Sendable {
    public let transport: SNMPTransport
    public var vendor: PDUVendor { .cyberPower }
    private enum Flavor { case ePDU2, legacy }
    private let lock = NSLock()
    private var detected: Flavor?
    /// Which per-outlet tables the device has, found at the first poll so that the next ones ask for these only.
    private var tables: Tables?
    private struct Tables { var switched: Bool; var metered: Bool; var monitored: Bool }
    public init(transport: SNMPTransport) { self.transport = transport }

    static let root = OID([1, 3, 6, 1, 4, 1, 3808, 1, 1, 6])
    static func oid(_ path: [UInt32], _ index: Int) -> OID { OID(root.components + path + [UInt32(index)]) }

    // ePDU2: single-row tables (index 1 is the PDU itself)
    static let model = [2, 2, 1, 9] as [UInt32], name = [2, 2, 1, 3] as [UInt32], serial = [2, 2, 1, 10] as [UInt32], firmware = [2, 2, 1, 7] as [UInt32]
    static let numOutlets = [3, 3, 1, 5] as [UInt32], numPhases = [3, 3, 1, 8] as [UInt32], numBreakers = [3, 3, 1, 9] as [UInt32]
    static let orientation = [3, 3, 1, 11] as [UInt32]
    static let deviceCurrent = [3, 4, 1, 5] as [UInt32]       // tenths of amps
    static let devicePower = [3, 4, 1, 18] as [UInt32]        // watts
    static let phaseLoad = [4, 4, 1, 5] as [UInt32], phaseVolts = [4, 4, 1, 6] as [UInt32], phasePower = [4, 4, 1, 7] as [UInt32]
    static let phaseLineToLine = [4, 4, 1, 13] as [UInt32]
    static let bankLoad = [5, 4, 1, 5] as [UInt32]
    // per outlet
    static let outletName = [6, 1, 4, 1, 4] as [UInt32], outletState = [6, 1, 4, 1, 5] as [UInt32]       // switched: 1 on, 2 off
    static let outletBank = [6, 1, 3, 1, 6] as [UInt32]
    static let meteredName = [6, 2, 4, 1, 4] as [UInt32], meteredLoad = [6, 2, 4, 1, 6] as [UInt32], meteredPower = [6, 2, 4, 1, 7] as [UInt32]
    static let meteredBank = [6, 2, 3, 1, 7] as [UInt32]
    static let monitoredName = [6, 3, 4, 1, 4] as [UInt32], monitoredBank = [6, 3, 3, 1, 7] as [UInt32]
    static let outletCommand = [6, 1, 5, 1, 5] as [UInt32]    // 1 on, 2 off (read: the same)

    // legacy ePDU (…3808.1.1.3)
    static let legacyRoot: [UInt32] = [1, 3, 6, 1, 4, 1, 3808, 1, 1, 3]
    static func legacy(_ path: [UInt32], _ index: Int) -> OID { OID(legacyRoot + path + [UInt32(index)]) }
    static func legacyScalar(_ path: [UInt32]) -> OID { OID(legacyRoot + path + [0]) }
    static let lName = [1, 1] as [UInt32], lFirmware = [1, 3] as [UInt32], lModel = [1, 5] as [UInt32], lSerial = [1, 6] as [UInt32]
    static let lNumOutlets = [1, 8] as [UInt32], lNumPhases = [1, 9] as [UInt32], lNumBreakers = [1, 10] as [UInt32], lLineVoltage = [1, 15] as [UInt32]
    static let lLoad = [2, 3, 1, 1, 2] as [UInt32], lLoadPhase = [2, 3, 1, 1, 4] as [UInt32], lLoadBank = [2, 3, 1, 1, 5] as [UInt32]
    static let lLoadVolts = [2, 3, 1, 1, 6] as [UInt32], lLoadPower = [2, 3, 1, 1, 7] as [UInt32]
    static let lControlName = [3, 3, 1, 1, 2] as [UInt32], lControlCommand = [3, 3, 1, 1, 4] as [UInt32], lControlBank = [3, 3, 1, 1, 5] as [UInt32]
    static let lStatusName = [3, 5, 1, 1, 2] as [UInt32], lStatusState = [3, 5, 1, 1, 4] as [UInt32], lStatusBank = [3, 5, 1, 1, 6] as [UInt32]
    static let lStatusLoad = [3, 5, 1, 1, 7] as [UInt32], lStatusPower = [3, 5, 1, 1, 8] as [UInt32]

    private func flavor() async throws -> Flavor {
        if let known = lock.withLock({ detected }) { return known }
        let probe = try await transport.getAvailable([Self.oid(Self.numOutlets, 1), Self.legacyScalar(Self.lNumOutlets)])
        let found: Flavor
        if (probe.int(Self.oid(Self.numOutlets, 1)) ?? 0) > 0 { found = .ePDU2 }
        else if (probe.int(Self.legacyScalar(Self.lNumOutlets)) ?? 0) > 0 { found = .legacy }
        else { throw DriverError.notRecognized("CyberPower") }
        lock.withLock { detected = found }
        return found
    }

    public func poll() async throws -> PDUSnapshot {
        switch try await flavor() {
        case .ePDU2: return try await pollEPDU2()
        case .legacy: return try await pollLegacy()
        }
    }

    public func setOutlet(_ number: Int, on: Bool) async throws {
        let oid = try await flavor() == .ePDU2 ? Self.oid(Self.outletCommand, number) : Self.legacy(Self.lControlCommand, number)
        try await transport.set([VarBind(oid, .integer(on ? 1 : 2))])
    }

    public func outletState(_ number: Int) async throws -> Bool? {
        let oid = try await flavor() == .ePDU2 ? Self.oid(Self.outletState, number) : Self.legacy(Self.lStatusState, number)
        guard let value = try await transport.getAvailable([oid]).int(oid) else { return nil }
        return value == 1
    }

    // MARK: ePDU2

    private func pollEPDU2() async throws -> PDUSnapshot {
        let head = try await transport.getAvailable([
            Self.oid(Self.model, 1), Self.oid(Self.name, 1), Self.oid(Self.serial, 1), Self.oid(Self.firmware, 1),
            Self.oid(Self.numOutlets, 1), Self.oid(Self.numPhases, 1), Self.oid(Self.numBreakers, 1), Self.oid(Self.orientation, 1),
            Self.oid(Self.deviceCurrent, 1), Self.oid(Self.devicePower, 1)
        ])
        guard let outletCount = head.int(Self.oid(Self.numOutlets, 1)), outletCount > 0 else { throw DriverError.notRecognized("CyberPower") }
        let phaseCount = min(3, max(1, head.int(Self.oid(Self.numPhases, 1)) ?? 1))
        let declaredBanks = head.int(Self.oid(Self.numBreakers, 1))
        let bankCount = min(8, declaredBanks ?? 0)
        let known = lock.withLock { tables }

        var wanted: [OID] = []
        for p in 1...phaseCount {
            wanted += [Self.oid(Self.phaseLoad, p), Self.oid(Self.phaseVolts, p), Self.oid(Self.phasePower, p), Self.oid(Self.phaseLineToLine, p)]
        }
        if bankCount > 0 { for b in 1...bankCount { wanted.append(Self.oid(Self.bankLoad, b)) } }
        for o in 1...outletCount {
            if known?.switched ?? true { wanted += [Self.oid(Self.outletName, o), Self.oid(Self.outletState, o), Self.oid(Self.outletBank, o)] }
            if known?.metered ?? true { wanted += [Self.oid(Self.meteredName, o), Self.oid(Self.meteredLoad, o), Self.oid(Self.meteredPower, o), Self.oid(Self.meteredBank, o)] }
            if known?.monitored ?? true { wanted += [Self.oid(Self.monitoredName, o), Self.oid(Self.monitoredBank, o)] }
        }
        let values = try await transport.getAvailable(wanted)
        if known == nil {
            let found = Tables(switched: values[Self.oid(Self.outletState, 1)] != nil,
                               metered: values[Self.oid(Self.meteredLoad, 1)] != nil || values[Self.oid(Self.meteredPower, 1)] != nil,
                               monitored: values[Self.oid(Self.monitoredName, 1)] != nil)
            lock.withLock { tables = found }
        }

        let phases: [PhaseReading] = (1...phaseCount).compactMap { p in
            guard let amps = values.tenths(Self.oid(Self.phaseLoad, p)) else { return nil }
            return PhaseReading(number: p, amps: amps, watts: values.int(Self.oid(Self.phasePower, p)).map(Double.init),
                                volts: values.tenths(Self.oid(Self.phaseVolts, p)))
        }
        let outlets: [OutletReading] = (1...outletCount).map { o in
            let name = values.string(Self.oid(Self.outletName, o)) ?? values.string(Self.oid(Self.meteredName, o)) ?? values.string(Self.oid(Self.monitoredName, o)) ?? ""
            let bank = values.int(Self.oid(Self.outletBank, o)) ?? values.int(Self.oid(Self.meteredBank, o)) ?? values.int(Self.oid(Self.monitoredBank, o))
            return OutletReading(number: o, name: name, bank: bank,
                                 isOn: values.int(Self.oid(Self.outletState, o)).map { $0 == 1 },
                                 amps: values.tenths(Self.oid(Self.meteredLoad, o)),
                                 watts: values.int(Self.oid(Self.meteredPower, o)).map(Double.init))
        }
        // Banks: the declared ones, or when the device does not declare any, the ones it answers for.
        var banks: [BankReading] = []
        if bankCount > 0 {
            banks = (1...bankCount).compactMap { b in values.tenths(Self.oid(Self.bankLoad, b)).map { BankReading(number: b, amps: $0) } }
        } else if declaredBanks == nil {
            let probe = try await transport.getAvailable((1...4).map { Self.oid(Self.bankLoad, $0) })
            banks = (1...4).compactMap { b in probe.tenths(Self.oid(Self.bankLoad, b)).map { BankReading(number: b, amps: $0) } }
        }
        let lineVoltage = values.tenths(Self.oid(Self.phaseLineToLine, 1)) ?? phases.first?.volts
        let info = PDUInfo(vendor: .cyberPower, model: head.string(Self.oid(Self.model, 1)), name: head.string(Self.oid(Self.name, 1)),
                           serial: head.string(Self.oid(Self.serial, 1)), firmware: head.string(Self.oid(Self.firmware, 1)),
                           outletCount: outletCount, breakerCount: banks.isEmpty ? nil : banks.count,
                           orientation: head.int(Self.oid(Self.orientation, 1)).map { $0 == 2 ? "Vertical" : "Horizontal" },
                           lineVoltage: lineVoltage)
        return PDUSnapshot(info: info, outlets: outlets, banks: banksWithWatts(banks, outlets: outlets), phases: phases,
                           totalAmps: phases.count > 1 ? nil : head.tenths(Self.oid(Self.deviceCurrent, 1)),
                           totalWatts: head.int(Self.oid(Self.devicePower, 1)).map(Double.init))
    }

    // MARK: older ePDU

    private func pollLegacy() async throws -> PDUSnapshot {
        let head = try await transport.getAvailable([
            Self.legacyScalar(Self.lName), Self.legacyScalar(Self.lFirmware), Self.legacyScalar(Self.lModel), Self.legacyScalar(Self.lSerial),
            Self.legacyScalar(Self.lNumOutlets), Self.legacyScalar(Self.lNumPhases), Self.legacyScalar(Self.lNumBreakers), Self.legacyScalar(Self.lLineVoltage)
        ])
        guard let outletCount = head.int(Self.legacyScalar(Self.lNumOutlets)), outletCount > 0 else { throw DriverError.notRecognized("CyberPower") }
        var wanted: [OID] = []
        // The load table lists the phases first and then the banks.
        for i in 1...10 { wanted += [Self.legacy(Self.lLoad, i), Self.legacy(Self.lLoadPhase, i), Self.legacy(Self.lLoadBank, i), Self.legacy(Self.lLoadVolts, i), Self.legacy(Self.lLoadPower, i)] }
        for o in 1...outletCount {
            wanted += [Self.legacy(Self.lControlName, o), Self.legacy(Self.lControlBank, o), Self.legacy(Self.lStatusName, o), Self.legacy(Self.lStatusState, o),
                       Self.legacy(Self.lStatusBank, o), Self.legacy(Self.lStatusLoad, o), Self.legacy(Self.lStatusPower, o)]
        }
        let values = try await transport.getAvailable(wanted)
        var phases: [PhaseReading] = [], banks: [BankReading] = []
        for i in 1...10 {
            guard let amps = values.tenths(Self.legacy(Self.lLoad, i)) else { continue }
            let bank = values.int(Self.legacy(Self.lLoadBank, i)) ?? 0
            if bank > 0 { banks.append(BankReading(number: bank, amps: amps, watts: values.int(Self.legacy(Self.lLoadPower, i)).map(Double.init))) }
            else {
                phases.append(PhaseReading(number: values.int(Self.legacy(Self.lLoadPhase, i)) ?? phases.count + 1, amps: amps,
                                           watts: values.int(Self.legacy(Self.lLoadPower, i)).map(Double.init), volts: values.tenths(Self.legacy(Self.lLoadVolts, i))))
            }
        }
        let outlets: [OutletReading] = (1...outletCount).map { o in
            let state = values.int(Self.legacy(Self.lStatusState, o))
            return OutletReading(number: o, name: values.string(Self.legacy(Self.lControlName, o)) ?? values.string(Self.legacy(Self.lStatusName, o)) ?? "",
                                 bank: values.int(Self.legacy(Self.lControlBank, o)) ?? values.int(Self.legacy(Self.lStatusBank, o)),
                                 isOn: state == 1 ? true : (state == 2 ? false : nil),
                                 amps: values.tenths(Self.legacy(Self.lStatusLoad, o)),
                                 watts: values.int(Self.legacy(Self.lStatusPower, o)).map(Double.init))
        }
        let info = PDUInfo(vendor: .cyberPower, model: head.string(Self.legacyScalar(Self.lModel)), name: head.string(Self.legacyScalar(Self.lName)),
                           serial: head.string(Self.legacyScalar(Self.lSerial)), firmware: head.string(Self.legacyScalar(Self.lFirmware)),
                           outletCount: outletCount, breakerCount: banks.isEmpty ? head.int(Self.legacyScalar(Self.lNumBreakers)) : banks.count,
                           lineVoltage: head.int(Self.legacyScalar(Self.lLineVoltage)).flatMap { $0 > 0 ? Double($0) : nil } ?? phases.first?.volts)
        return PDUSnapshot(info: info, outlets: outlets, banks: banksWithWatts(banks.sorted { $0.number < $1.number }, outlets: outlets), phases: phases)
    }
}
