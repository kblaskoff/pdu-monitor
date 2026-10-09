import Foundation

/// A PDU that exists only in memory: for the demo mode of the application and for tests. Outlets can be switched.
public final class SimulatedPDU: PDUDriver, @unchecked Sendable {
    public struct Load: Sendable { public var amps: Double; public init(amps: Double) { self.amps = amps } }
    public let vendor: PDUVendor
    private let lock = NSLock()
    private var names: [Int: String]
    private var loads: [Int: Load]
    private var states: [Int: Bool] = [:]
    private let outletCount: Int
    private let banks: Int
    private let volts: Double
    private let model: String
    private let metered: Bool
    private var tick = 0

    /// `outlets` maps an outlet number to its name and the amps it draws while on.
    public init(vendor: PDUVendor, model: String, outletCount: Int, banks: Int = 2, volts: Double = 230, metered: Bool = true,
                outlets: [Int: (name: String, amps: Double)]) {
        self.vendor = vendor; self.model = model; self.outletCount = outletCount; self.banks = banks; self.volts = volts; self.metered = metered
        names = outlets.mapValues(\.name); loads = outlets.mapValues { Load(amps: $0.amps) }
        for n in 1...outletCount { states[n] = true }
    }

    public func poll() async throws -> PDUSnapshot { makeSnapshot() }

    private func makeSnapshot() -> PDUSnapshot {
        lock.lock(); defer { lock.unlock() }
        tick += 1
        var outlets: [OutletReading] = []
        var truth: [Int: Double] = [:]          // what the outlet really draws, also when the PDU cannot measure it
        for n in 1...outletCount {
            let on = states[n] ?? true
            let base = on ? (loads[n]?.amps ?? 0) : 0
            // A little movement so that the screens look alive.
            let wobble = base > 0 ? sin(Double(tick + n) / 3) * 0.05 * base : 0
            let amps = max(0, ((base + wobble) * 10).rounded() / 10)
            truth[n] = amps
            outlets.append(OutletReading(number: n, name: names[n] ?? "Outlet_\(n)", bank: (n - 1) * banks / outletCount + 1, isOn: on,
                                         amps: metered ? amps : nil, watts: metered ? (amps * volts * 0.95).rounded() : nil))
        }
        var bankReadings: [BankReading] = []
        for b in 1...banks {
            let sum = outlets.filter { $0.bank == b }.reduce(0.0) { $0 + (truth[$1.number] ?? 0) }
            bankReadings.append(BankReading(number: b, amps: (sum * 10).rounded() / 10))
        }
        let total = bankReadings.reduce(0) { $0 + $1.amps }
        let watts = (total * volts * 0.95).rounded()
        let info = PDUInfo(vendor: vendor, model: model, name: nil, serial: nil, firmware: nil, outletCount: outletCount, breakerCount: banks,
                           orientation: outletCount > 16 ? "Vertical" : "Horizontal", lineVoltage: volts)
        let phase = PhaseReading(number: 1, amps: total, watts: watts, volts: volts)
        return PDUSnapshot(info: info, outlets: outlets, banks: banksWithWatts(bankReadings, outlets: outlets), phases: [phase],
                           totalAmps: total, totalWatts: watts)
    }

    public func setOutlet(_ number: Int, on: Bool) async throws { lock.withLock { states[number] = on } }
    public func outletState(_ number: Int) async throws -> Bool? { lock.withLock { states[number] } }
}

/// Sample racks for the demo mode (modelled on the screens of the Python tool this application replaces).
public enum DemoLab {
    public struct Lab {
        public var racks: [RackConfig]
        public var devices: [DeviceConfig]
        public var drivers: [UUID: PDUDriver]
    }

    public static func make() -> Lab {
        var racks: [RackConfig] = [], devices: [DeviceConfig] = [], drivers: [UUID: PDUDriver] = [:]
        let layout: [(rack: String, limit: Double, servers: [(String, Double)])] = [
            ("100", 24, [("100U31", 0.8), ("100U29", 0.4), ("100U24", 1.9), ("100U22", 2.0), ("N100U43", 0.3), ("100U12", 1.6), ("100U08", 1.2), ("100U04", 0.5)]),
            ("200", 24, [("200U31", 0.5), ("200U29", 0.9), ("200U27", 1.0), ("200U24", 0.9), ("200U22", 1.2), ("N200U43", 0.3), ("N200U44", 0.4), ("200U20", 3.5),
                         ("200U18", 3.7), ("200U16", 3.6), ("200U13", 3.2), ("200U12", 0.4), ("200U03", 1.9)]),
            ("20F", 24, [("20FU38", 4.5), ("20FU33", 4.2), ("20FU28", 4.0), ("20FU22", 3.9), ("20FU16", 3.7), ("20FU10", 3.3), ("20FU04", 4.4)])
        ]
        for entry in layout {
            let rack = RackConfig(name: entry.rack, maxAmps: entry.limit)
            racks.append(rack)
            // Every server draws half of its current on each of the two PDUs.
            for (index, suffix) in ["A", "B"].enumerated() {
                var outlets: [Int: (name: String, amps: Double)] = [:]
                for (i, server) in entry.servers.enumerated() { outlets[i + 1] = (server.0, server.1 / 2) }
                let count = index == 0 ? 24 : 16
                let config = DeviceConfig(name: "P\(entry.rack)\(suffix)", rackID: rack.id, vendor: index == 0 ? .apc : .cyberPower,
                                          host: "demo-\(entry.rack)\(suffix.lowercased())", maxAmps: 24)
                devices.append(config)
                drivers[config.id] = SimulatedPDU(vendor: config.vendor, model: index == 0 ? "AP7932" : "PDU81007", outletCount: count,
                                                  metered: index != 0, outlets: outlets)
            }
        }
        return Lab(racks: racks, devices: devices, drivers: drivers)
    }
}
