import SwiftUI
import UserNotifications
import PDUCore

/// Maps the ports of the power sequencer to the drivers of the PDUs. Thread-safe: the sequencer runs off the main thread.
final class DriverBook: OutletController, @unchecked Sendable {
    private let lock = NSLock()
    private var drivers: [UUID: PDUDriver] = [:]

    func replace(_ new: [UUID: PDUDriver]) { lock.withLock { drivers = new } }
    func driver(_ id: UUID) -> PDUDriver? { lock.withLock { drivers[id] } }

    func set(_ target: OutletTarget, on: Bool) async throws {
        guard let driver = driver(target.pduID) else { throw SNMPError.cannotResolve("PDU") }
        try await driver.setOutlet(target.outlet, on: on)
    }
    func state(_ target: OutletTarget) async throws -> Bool? {
        guard let driver = driver(target.pduID) else { return nil }
        return try await driver.outletState(target.outlet)
    }
}

/// What the banner at the bottom of the window shows while ports are being switched.
struct OperationState: Identifiable {
    let id = UUID()
    var title: String
    var detail: String
    var running: Bool
    var error: String?
}

enum Route: Hashable {
    case overview
    case rack(UUID)
    case pdu(UUID)
}

@MainActor
final class AppModel: ObservableObject {
    @Published var racks: [RackConfig] = []
    @Published var devices: [DeviceConfig] = []
    @Published var labels = ServerLabels()
    @Published var settings = AppSettings() { didSet { settingsChanged(from: oldValue) } }
    @Published private(set) var states: [UUID: PDUState] = [:]
    @Published private(set) var lastPoll: Date?
    @Published private(set) var polling = false
    @Published var operation: OperationState?
    @Published var route: Route = .overview

    let updater = AppUpdater()
    private let book = DriverBook()
    private var demoDrivers: [UUID: PDUDriver] = [:]
    private var stash: (racks: [RackConfig], devices: [DeviceConfig])?
    private var pollTask: Task<Void, Never>?
    private var attention: [UUID: LimitStatus] = [:]
    private let history: HistoryStore = NullHistoryStore()

    init() {
        DebugLog.start()
        var file = ConfigStore.load()
        for i in file.devices.indices { Secrets.load(into: &file.devices[i]) }
        racks = file.racks; devices = file.devices; labels = file.labels
        let loaded = file.settings
        settings = loaded
        if loaded.demoMode { enterDemo() }
        rebuildDrivers()
        startPolling()
        if settings.notifyWhenOver { Notifier.requestAuthorization() }
    }

    // MARK: derived

    func devices(in rack: UUID) -> [DeviceConfig] { devices.filter { $0.rackID == rack } }
    func pduStates(in rack: UUID) -> [PDUState] { devices(in: rack).map { states[$0.id] ?? PDUState(config: $0) } }
    func summary(of rack: RackConfig) -> RackSummary {
        RackSummarizer.summary(rack: rack, pdus: pduStates(in: rack.id), warnFraction: settings.warnFraction)
    }
    func servers(in rack: UUID) -> [ServerEntry] { RackAggregator.servers(of: pduStates(in: rack)) }
    func rack(_ id: UUID) -> RackConfig? { racks.first { $0.id == id } }
    func device(_ id: UUID) -> DeviceConfig? { devices.first { $0.id == id } }
    func displayName(of server: ServerEntry, rack: UUID) -> String { server.name }
    func label(of server: ServerEntry, rack: UUID) -> String? { labels.label(rack: rack, id: server.name) }

    /// Racks in the order the overview shows them.
    var orderedRacks: [RackConfig] {
        guard settings.sortByLoad else { return racks }
        return racks.sorted { a, b in
            let sa = summary(of: a), sb = summary(of: b)
            if sa.attention != sb.attention { return sa.attention > sb.attention }
            return (sa.fraction ?? -1) > (sb.fraction ?? -1)
        }
    }

    var totalAmps: Double? { let v = racks.compactMap { summary(of: $0).amps }; return v.isEmpty ? nil : v.reduce(0, +) }
    var totalWatts: Double? { let v = racks.compactMap { summary(of: $0).watts }; return v.isEmpty ? nil : v.reduce(0, +) }
    var racksNeedingAttention: Int { racks.filter { summary(of: $0).attention == .over }.count }

    // MARK: editing

    func addRack(_ rack: RackConfig) { racks.append(rack); save() }
    func updateRack(_ rack: RackConfig) {
        guard let i = racks.firstIndex(where: { $0.id == rack.id }) else { return }
        racks[i] = rack; save()
    }
    func deleteRack(_ id: UUID) {
        for d in devices where d.rackID == id { removeDeviceData(d.id) }
        devices.removeAll { $0.rackID == id }
        racks.removeAll { $0.id == id }
        labels.labels.removeValue(forKey: id.uuidString)
        if case .rack(id) = route { route = .overview }
        rebuildDrivers(); save()
    }

    func addDevice(_ device: DeviceConfig) {
        devices.append(device)
        if !settings.demoMode { Secrets.save(device) }
        rebuildDrivers(); save()
        Task { await poll(only: [device.id]) }
    }
    func updateDevice(_ device: DeviceConfig) {
        guard let i = devices.firstIndex(where: { $0.id == device.id }) else { return }
        devices[i] = device
        if !settings.demoMode { Secrets.save(device) }
        states[device.id] = nil
        rebuildDrivers(); save()
        Task { await poll(only: [device.id]) }
    }
    func deleteDevice(_ id: UUID) {
        removeDeviceData(id)
        devices.removeAll { $0.id == id }
        if case .pdu(id) = route { route = .overview }
        rebuildDrivers(); save()
    }
    private func removeDeviceData(_ id: UUID) {
        states[id] = nil
        if !settings.demoMode { Secrets.remove(id) }
    }

    func setLabel(_ text: String, rack: UUID, server: ServerEntry) { labels.set(text, rack: rack, id: server.name); save() }

    // MARK: demo

    private func enterDemo() {
        guard stash == nil else { return }
        stash = (racks, devices)
        let lab = DemoLab.make()
        racks = lab.racks; devices = lab.devices; demoDrivers = lab.drivers
        states = [:]; route = .overview
    }
    private func leaveDemo() {
        guard let saved = stash else { return }
        racks = saved.racks; devices = saved.devices; stash = nil; demoDrivers = [:]
        states = [:]; route = .overview
    }

    private func settingsChanged(from old: AppSettings) {
        if old.demoMode != settings.demoMode {
            if settings.demoMode { enterDemo() } else { leaveDemo() }
            rebuildDrivers()
            Task { await poll() }
        }
        if old.pollInterval != settings.pollInterval { startPolling() }
        if settings.notifyWhenOver && !old.notifyWhenOver { Notifier.requestAuthorization() }
        save()
    }

    func save() {
        var file = ConfigFile()
        file.racks = stash?.racks ?? racks
        file.devices = stash?.devices ?? devices
        file.labels = labels
        file.settings = settings
        ConfigStore.save(file)
    }

    // MARK: polling

    var stashedRacks: [RackConfig]? { stash?.racks }
    var stashedDevices: [DeviceConfig]? { stash?.devices }
    func rebuildDriversAfterRestore() { rebuildDrivers() }

    private func rebuildDrivers() {
        if settings.demoMode { book.replace(demoDrivers); return }
        var made: [UUID: PDUDriver] = [:]
        for d in devices {
            let client = UDPSNMPClient(host: d.host, port: UInt16(clamping: d.port), readCommunity: d.readCommunity,
                                       writeCommunity: d.writeCommunity.isEmpty ? nil : d.writeCommunity, timeout: 2, retries: 1)
            made[d.id] = PDUDriverFactory.make(vendor: d.vendor, transport: client)
        }
        book.replace(made)
    }

    func startPolling() {
        pollTask?.cancel()
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                guard let self else { return }
                await self.poll()
                let seconds = max(3, self.settings.pollInterval)
                try? await Task.sleep(nanoseconds: UInt64(seconds * 1_000_000_000))
            }
        }
    }

    func poll(only ids: [UUID]? = nil) async {
        let wanted = devices.filter { ids?.contains($0.id) ?? true }
        guard !wanted.isEmpty else { return }
        if ids == nil { polling = true }
        let book = self.book
        let results = await withTaskGroup(of: (UUID, Result<PDUSnapshot, Error>).self) { group -> [(UUID, Result<PDUSnapshot, Error>)] in
            for d in wanted {
                group.addTask {
                    guard let driver = book.driver(d.id) else { return (d.id, .failure(SNMPError.cannotResolve(d.host))) }
                    do { return (d.id, .success(try await driver.poll())) } catch { return (d.id, .failure(error)) }
                }
            }
            var all: [(UUID, Result<PDUSnapshot, Error>)] = []
            for await r in group { all.append(r) }
            return all
        }
        var samples: [PowerSample] = []
        for (id, result) in results {
            guard let config = device(id) else { continue }
            var state = states[id] ?? PDUState(config: config)
            state.config = config
            switch result {
            case .success(let snapshot):
                state.snapshot = snapshot; state.error = nil
                samples.append(PowerSample(pduID: id, rackID: config.rackID, timestamp: snapshot.timestamp, amps: snapshot.totalAmps, watts: snapshot.totalWatts))
            case .failure(let error):
                // The last good reading is kept (greyed out on the screens) so that one lost packet does not blank the window.
                state.error = error.localizedDescription
                DebugLog.write("read of \(config.name) (\(config.vendor.displayName) \(config.host):\(config.port)) failed: \(error.localizedDescription) [\(error)]")
            }
            states[id] = state
        }
        if ids == nil { polling = false; lastPoll = Date() }
        if !samples.isEmpty { let h = history; Task { await h.record(samples) } }
        checkLimits()
    }

    private func checkLimits() {
        for rack in racks {
            let now = summary(of: rack).attention
            let before = attention[rack.id]
            attention[rack.id] = now
            if now == .over, before != .over, before != nil, settings.notifyWhenOver {
                let s = summary(of: rack)
                Notifier.post(title: "Rack \(rack.name) is over its limit",
                              body: String(format: "%.1f A of %.0f A", s.amps ?? 0, rack.maxAmps))
            }
        }
    }

    // MARK: switching ports

    /// True while ports are being switched: a second operation must wait.
    var busy: Bool { operation?.running == true }

    /// The lines of the confirmation: which port of which PDU.
    func describe(_ targets: [OutletTarget]) -> [String] {
        targets.map { t in
            let pdu = states[t.pduID]
            let name = pdu?.snapshot?.outlets.first { $0.number == t.outlet }?.name
            let pduName = device(t.pduID)?.name ?? "PDU"
            let shown = (name?.isEmpty == false && !ServerID.isDefaultOutletName(name ?? "")) ? "\(name!) — " : ""
            return "\(shown)\(pduName) outlet \(t.outlet)"
        }
    }

    func perform(_ operation: PowerOperation, targets: [OutletTarget], title: String) {
        guard !busy, !targets.isEmpty else { return }
        self.operation = OperationState(title: title, detail: "Starting…", running: true)
        let book = self.book
        let names: [OutletTarget: String] = Dictionary(uniqueKeysWithValues: targets.map { ($0, describe([$0])[0]) })
        let sequencer = PowerSequencer(controller: book, label: { names[$0] ?? "outlet \($0.outlet)" })
        Task { [weak self] in
            do {
                try await sequencer.run(operation, targets: targets) { progress in
                    Task { @MainActor [weak self] in self?.operation?.detail = Self.text(progress) }
                }
                self?.operation?.running = false
                self?.operation?.detail = "Done"
            } catch {
                self?.operation?.running = false
                self?.operation?.error = error.localizedDescription
            }
            await self?.poll(only: Array(Set(targets.map(\.pduID))))
            // A finished operation stays visible for a moment, a failed one until it is dismissed.
            if self?.operation?.error == nil {
                try? await Task.sleep(nanoseconds: 4_000_000_000)
                if self?.operation?.running == false, self?.operation?.error == nil { self?.operation = nil }
            }
        }
    }

    private static func text(_ progress: PowerProgress) -> String {
        switch progress {
        case .switchingOff: return "Switching off…"
        case .confirmedOff: return "Off confirmed by the PDU"
        case .waiting(let left): return "Waiting… \(left) s"
        case .switchingOn: return "Switching on…"
        case .confirmedOn: return "On confirmed by the PDU"
        case .rolledBack: return "Switching back on…"
        }
    }

    /// Tries a connection with the values typed in the editor (nothing is saved).
    func test(_ device: DeviceConfig) async -> Result<PDUSnapshot, Error> {
        let client = UDPSNMPClient(host: device.host, port: UInt16(clamping: device.port), readCommunity: device.readCommunity,
                                   writeCommunity: nil, timeout: 2, retries: 1)
        DebugLog.write("test of \(device.name) (\(device.vendor.displayName) \(device.host):\(device.port))")
        do { return .success(try await PDUDriverFactory.make(vendor: device.vendor, transport: client).poll()) }
        catch {
            DebugLog.write("test of \(device.name) failed: \(error.localizedDescription) [\(error)]")
            return .failure(error)
        }
    }
}

enum Notifier {
    private static var available: Bool { Bundle.main.bundleURL.pathExtension == "app" }
    static func requestAuthorization() {
        guard available else { return }
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound]) { _, _ in }
    }
    static func post(title: String, body: String) {
        guard available else { return }
        let content = UNMutableNotificationContent()
        content.title = title; content.body = body; content.sound = .default
        UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil))
    }
}
