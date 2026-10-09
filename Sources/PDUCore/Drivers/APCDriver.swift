import Foundation

/// APC Rack PDUs (AP7932 and the like). Newer firmware speaks the rPDU2 MIB (1.3.6.1.4.1.318.1.1.26); older firmware only the
/// original rPDU / sPDU MIBs (…318.1.1.12 and …318.1.1.4). The driver asks the device which one it has and remembers the answer.
public final class APCDriver: PDUDriver, @unchecked Sendable {
    public let transport: SNMPTransport
    public var vendor: PDUVendor { .apc }
    private enum Flavor { case rPDU2, legacy }
    private let lock = NSLock()
    private var detected: Flavor?
    public init(transport: SNMPTransport) { self.transport = transport }

    static let apc: [UInt32] = [1, 3, 6, 1, 4, 1, 318, 1, 1]
    static func oid(_ path: [UInt32], _ index: Int) -> OID { OID(apc + path + [UInt32(index)]) }

    // rPDU2 (…318.1.1.26)
    static let r2Name = [26, 2, 1, 3] as [UInt32], r2Firmware = [26, 2, 1, 6] as [UInt32], r2Model = [26, 2, 1, 8] as [UInt32], r2Serial = [26, 2, 1, 9] as [UInt32]
    static let r2NumOutlets = [26, 4, 2, 1, 4] as [UInt32], r2NumPhases = [26, 4, 2, 1, 7] as [UInt32], r2NumBanks = [26, 4, 2, 1, 8] as [UInt32]
    static let r2Orientation = [26, 4, 2, 1, 10] as [UInt32]
    static let r2DevicePower = [26, 4, 3, 1, 5] as [UInt32]          // hundredths of kW, -1 = unsupported
    static let r2PhaseCurrent = [26, 6, 3, 1, 5] as [UInt32], r2PhaseVolts = [26, 6, 3, 1, 6] as [UInt32], r2PhasePower = [26, 6, 3, 1, 7] as [UInt32]
    static let r2BankCurrent = [26, 8, 3, 1, 5] as [UInt32]
    static let r2OutletName = [26, 9, 2, 3, 1, 3] as [UInt32], r2OutletState = [26, 9, 2, 3, 1, 5] as [UInt32]   // 1 off, 2 on
    static let r2OutletBank = [26, 9, 2, 2, 1, 6] as [UInt32]
    static let r2OutletCommand = [26, 9, 2, 4, 1, 5] as [UInt32]       // 1 on, 2 off
    static let r2MeteredCurrent = [26, 9, 4, 3, 1, 6] as [UInt32], r2MeteredPower = [26, 9, 4, 3, 1, 7] as [UInt32]

    // rPDU / sPDU (older firmware)
    static let lName = [12, 1, 1] as [UInt32], lFirmware = [12, 1, 3] as [UInt32], lModel = [12, 1, 5] as [UInt32], lSerial = [12, 1, 6] as [UInt32]
    static let lNumOutlets = [12, 1, 8] as [UInt32], lNumPhases = [12, 1, 9] as [UInt32], lPowerWatts = [12, 1, 16] as [UInt32]
    static let lLoad = [12, 2, 3, 1, 1, 2] as [UInt32], lLoadPhase = [12, 2, 3, 1, 1, 4] as [UInt32], lLoadBank = [12, 2, 3, 1, 1, 5] as [UInt32]
    static let sOutletName = [4, 4, 2, 1, 4] as [UInt32], sOutletCommand = [4, 4, 2, 1, 3] as [UInt32]               // 1 on, 2 off
    static let lOutletBank = [12, 3, 5, 1, 1, 6] as [UInt32], lOutletLoad = [12, 3, 5, 1, 1, 7] as [UInt32]

    /// A scalar object of the older MIBs ends in .0
    static func scalar(_ path: [UInt32]) -> OID { OID(apc + path + [0]) }

    private func flavor() async throws -> Flavor {
        if let known = lock.withLock({ detected }) { return known }
        let probe = try await transport.getAvailable([Self.oid(Self.r2NumOutlets, 1), Self.scalar(Self.lNumOutlets)])
        let found: Flavor
        if (probe.int(Self.oid(Self.r2NumOutlets, 1)) ?? 0) > 0 { found = .rPDU2 }
        else if (probe.int(Self.scalar(Self.lNumOutlets)) ?? 0) > 0 { found = .legacy }
        else { throw DriverError.notRecognized("APC") }
        lock.withLock { detected = found }
        return found
    }

    public func poll() async throws -> PDUSnapshot {
        switch try await flavor() {
        case .rPDU2: return try await pollRPDU2()
        case .legacy: return try await pollLegacy()
        }
    }

    public func setOutlet(_ number: Int, on: Bool) async throws {
        let oid = try await flavor() == .rPDU2 ? Self.oid(Self.r2OutletCommand, number) : Self.oid(Self.sOutletCommand, number)
        try await transport.set([VarBind(oid, .integer(on ? 1 : 2))])
    }

    public func outletState(_ number: Int) async throws -> Bool? {
        if try await flavor() == .rPDU2 {
            let oid = Self.oid(Self.r2OutletState, number)
            return try await transport.getAvailable([oid]).int(oid).map { $0 == 2 }        // here 2 means on
        }
        let oid = Self.oid(Self.sOutletCommand, number)
        guard let value = try await transport.getAvailable([oid]).int(oid), value == 1 || value == 2 else { return nil }
        return value == 1
    }

    // MARK: rPDU2

    private func pollRPDU2() async throws -> PDUSnapshot {
        let head = try await transport.getAvailable([
            Self.oid(Self.r2Name, 1), Self.oid(Self.r2Firmware, 1), Self.oid(Self.r2Model, 1), Self.oid(Self.r2Serial, 1),
            Self.oid(Self.r2NumOutlets, 1), Self.oid(Self.r2NumPhases, 1), Self.oid(Self.r2NumBanks, 1), Self.oid(Self.r2Orientation, 1),
            Self.oid(Self.r2DevicePower, 1)
        ])
        guard let outletCount = head.int(Self.oid(Self.r2NumOutlets, 1)), outletCount > 0 else { throw DriverError.notRecognized("APC") }
        let phaseCount = max(1, head.int(Self.oid(Self.r2NumPhases, 1)) ?? 1)
        let bankCount = head.int(Self.oid(Self.r2NumBanks, 1)) ?? 0
        var wanted: [OID] = []
        for p in 1...phaseCount { wanted += [Self.oid(Self.r2PhaseCurrent, p), Self.oid(Self.r2PhaseVolts, p), Self.oid(Self.r2PhasePower, p)] }
        if bankCount > 0 { for b in 1...bankCount { wanted.append(Self.oid(Self.r2BankCurrent, b)) } }
        for o in 1...outletCount {
            wanted += [Self.oid(Self.r2OutletName, o), Self.oid(Self.r2OutletState, o), Self.oid(Self.r2OutletBank, o),
                       Self.oid(Self.r2MeteredCurrent, o), Self.oid(Self.r2MeteredPower, o)]
        }
        let values = try await transport.getAvailable(wanted)
        let phases: [PhaseReading] = (1...phaseCount).compactMap { p in
            guard let amps = values.tenths(Self.oid(Self.r2PhaseCurrent, p)) else { return nil }
            let kw = values.int(Self.oid(Self.r2PhasePower, p)).flatMap { $0 >= 0 ? Double($0) * 10 : nil }
            let volts = values.int(Self.oid(Self.r2PhaseVolts, p)).flatMap { $0 >= 0 ? Double($0) : nil }
            return PhaseReading(number: p, amps: amps, watts: kw, volts: volts)
        }
        let outlets: [OutletReading] = (1...outletCount).map { o in
            OutletReading(number: o, name: values.string(Self.oid(Self.r2OutletName, o)) ?? "",
                          bank: values.int(Self.oid(Self.r2OutletBank, o)),
                          isOn: values.int(Self.oid(Self.r2OutletState, o)).map { $0 == 2 },
                          amps: values.tenths(Self.oid(Self.r2MeteredCurrent, o)),
                          watts: values.int(Self.oid(Self.r2MeteredPower, o)).flatMap { $0 >= 0 ? Double($0) : nil })
        }
        var banks: [BankReading] = []
        if bankCount > 0 { banks = (1...bankCount).compactMap { b in values.tenths(Self.oid(Self.r2BankCurrent, b)).map { BankReading(number: b, amps: $0) } } }
        let orientation: String? = head.int(Self.oid(Self.r2Orientation, 1)).map { $0 == 1 ? "Horizontal" : "Vertical" }
        let info = PDUInfo(vendor: .apc, model: head.string(Self.oid(Self.r2Model, 1)), name: head.string(Self.oid(Self.r2Name, 1)),
                           serial: head.string(Self.oid(Self.r2Serial, 1)), firmware: head.string(Self.oid(Self.r2Firmware, 1)),
                           outletCount: outletCount, breakerCount: bankCount > 0 ? bankCount : nil, orientation: orientation,
                           lineVoltage: phases.first?.volts)
        let deviceWatts = head.int(Self.oid(Self.r2DevicePower, 1)).flatMap { $0 >= 0 ? Double($0) * 10 : nil }
        return PDUSnapshot(info: info, outlets: outlets, banks: banksWithWatts(banks, outlets: outlets), phases: phases, totalWatts: deviceWatts)
    }

    // MARK: older firmware

    private func pollLegacy() async throws -> PDUSnapshot {
        let head = try await transport.getAvailable([
            Self.scalar(Self.lName), Self.scalar(Self.lFirmware), Self.scalar(Self.lModel), Self.scalar(Self.lSerial),
            Self.scalar(Self.lNumOutlets), Self.scalar(Self.lNumPhases), Self.scalar(Self.lPowerWatts)
        ])
        guard let outletCount = head.int(Self.scalar(Self.lNumOutlets)), outletCount > 0 else { throw DriverError.notRecognized("APC") }
        var wanted: [OID] = []
        // The load table lists the phases first and then the banks.
        for i in 1...8 { wanted += [Self.oid(Self.lLoad, i), Self.oid(Self.lLoadPhase, i), Self.oid(Self.lLoadBank, i)] }
        for o in 1...outletCount {
            wanted += [Self.oid(Self.sOutletName, o), Self.oid(Self.sOutletCommand, o), Self.oid(Self.lOutletBank, o), Self.oid(Self.lOutletLoad, o)]
        }
        let values = try await transport.getAvailable(wanted)
        var phases: [PhaseReading] = [], banks: [BankReading] = []
        for i in 1...8 {
            guard let amps = values.tenths(Self.oid(Self.lLoad, i)) else { continue }
            let bank = values.int(Self.oid(Self.lLoadBank, i)) ?? 0
            if bank > 0 { banks.append(BankReading(number: bank, amps: amps)) }
            else { phases.append(PhaseReading(number: values.int(Self.oid(Self.lLoadPhase, i)) ?? phases.count + 1, amps: amps)) }
        }
        let outlets: [OutletReading] = (1...outletCount).map { o in
            let state = values.int(Self.oid(Self.sOutletCommand, o))
            return OutletReading(number: o, name: values.string(Self.oid(Self.sOutletName, o)) ?? "",
                                 bank: values.int(Self.oid(Self.lOutletBank, o)),
                                 isOn: state == 1 ? true : (state == 2 ? false : nil),
                                 amps: values.tenths(Self.oid(Self.lOutletLoad, o)), watts: nil)
        }
        let info = PDUInfo(vendor: .apc, model: head.string(Self.scalar(Self.lModel)), name: head.string(Self.scalar(Self.lName)),
                           serial: head.string(Self.scalar(Self.lSerial)), firmware: head.string(Self.scalar(Self.lFirmware)),
                           outletCount: outletCount, breakerCount: banks.isEmpty ? nil : banks.count)
        return PDUSnapshot(info: info, outlets: outlets, banks: banks.sorted { $0.number < $1.number }, phases: phases,
                           totalWatts: head.int(Self.scalar(Self.lPowerWatts)).flatMap { $0 >= 0 ? Double($0) : nil })
    }
}
