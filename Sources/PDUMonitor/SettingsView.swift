import SwiftUI
import PDUCore

struct SettingsView: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        TabView {
            RacksSettings().tabItem { Label("Racks", systemImage: "server.rack") }
            DevicesSettings().tabItem { Label("PDUs", systemImage: "powerplug") }
            GeneralSettings().tabItem { Label("General", systemImage: "gearshape") }
        }
        .frame(width: 640, height: 460)
    }
}

// MARK: - Racks

struct RacksSettings: View {
    @EnvironmentObject var model: AppModel
    @State private var confirmDelete: RackConfig?

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("A rack groups its PDUs. The limit is for all the PDUs of the rack together (usually 24 A).")
                .font(.callout).foregroundStyle(.secondary)
            if model.settings.demoMode {
                Label("Demo mode is on: these are sample racks. Turn it off in General to edit your own.", systemImage: "play.rectangle").foregroundStyle(.orange)
            }
            List {
                ForEach($model.racks) { $rack in
                    HStack {
                        TextField("Name", text: $rack.name).frame(minWidth: 140)
                        Spacer()
                        Text("Limit").foregroundStyle(.secondary)
                        TextField("24", value: $rack.maxAmps, format: .number).frame(width: 60).multilineTextAlignment(.trailing)
                        Text("A").foregroundStyle(.secondary)
                        Button(role: .destructive) { confirmDelete = rack } label: { Image(systemName: "trash") }.buttonStyle(.borderless)
                    }
                }
            }
            .onChange(of: model.racks) { _, _ in model.save() }
            HStack {
                Button { model.addRack(RackConfig(name: "Rack \(model.racks.count + 1)")) } label: { Label("Add rack", systemImage: "plus") }
                    .disabled(model.settings.demoMode)
                Spacer()
            }
        }
        .padding(20)
        .alert("Delete rack \(confirmDelete?.name ?? "")?", isPresented: Binding(get: { confirmDelete != nil }, set: { if !$0 { confirmDelete = nil } }), presenting: confirmDelete) { rack in
            Button("Cancel", role: .cancel) {}
            Button("Delete", role: .destructive) { model.deleteRack(rack.id) }
        } message: { rack in
            Text("Its \(model.devices(in: rack.id).count) PDU(s) are removed from this application. Nothing changes on the PDUs.")
        }
    }
}

// MARK: - PDUs

struct DevicesSettings: View {
    @EnvironmentObject var model: AppModel
    @State private var editing: DeviceConfig?
    @State private var isNew = false
    @State private var confirmDelete: DeviceConfig?

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("SNMP v1 is used. The community string is kept in the macOS Keychain.").font(.callout).foregroundStyle(.secondary)
            if model.settings.demoMode {
                Label("Demo mode is on: these are sample PDUs. Turn it off in General to edit your own.", systemImage: "play.rectangle").foregroundStyle(.orange)
            }
            List {
                ForEach(model.devices) { device in
                    HStack {
                        VStack(alignment: .leading, spacing: 2) {
                            Text(device.name).font(.body.weight(.semibold))
                            Text("\(device.vendor.displayName) · \(device.host) · rack \(model.rack(device.rackID)?.name ?? "?")")
                                .font(.caption).foregroundStyle(.secondary)
                        }
                        Spacer()
                        if let error = model.states[device.id]?.error {
                            Image(systemName: "wifi.exclamationmark").foregroundStyle(.orange).help(error)
                        }
                        Button("Edit…") { isNew = false; editing = device }
                        Button(role: .destructive) { confirmDelete = device } label: { Image(systemName: "trash") }.buttonStyle(.borderless)
                    }
                }
            }
            HStack {
                Button {
                    guard let rack = model.racks.first else { return }
                    isNew = true
                    editing = DeviceConfig(name: "", rackID: rack.id, vendor: .cyberPower, host: "")
                } label: { Label("Add PDU…", systemImage: "plus") }
                    .disabled(model.racks.isEmpty || model.settings.demoMode)
                if model.racks.isEmpty { Text("Add a rack first.").foregroundStyle(.secondary) }
                Spacer()
            }
        }
        .padding(20)
        .sheet(item: $editing) { device in DeviceEditor(device: device, isNew: isNew) }
        .alert("Delete PDU \(confirmDelete?.name ?? "")?", isPresented: Binding(get: { confirmDelete != nil }, set: { if !$0 { confirmDelete = nil } }), presenting: confirmDelete) { device in
            Button("Cancel", role: .cancel) {}
            Button("Delete", role: .destructive) { model.deleteDevice(device.id) }
        } message: { _ in
            Text("It is removed from this application only. Nothing changes on the PDU.")
        }
    }
}

struct DeviceEditor: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) private var dismiss
    @State var device: DeviceConfig
    let isNew: Bool
    @State private var bankLimitText = ""
    @State private var testing = false
    @State private var testResult: String?
    @State private var testOK = false

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Form {
                Section {
                    TextField("Name", text: $device.name, prompt: Text("P200A"))
                    Picker("Rack", selection: $device.rackID) { ForEach(model.racks) { Text($0.name).tag($0.id) } }
                    Picker("Vendor", selection: $device.vendor) { ForEach(PDUVendor.allCases) { Text($0.displayName).tag($0) } }
                }
                Section("Network (SNMP v1)") {
                    TextField("Address", text: $device.host, prompt: Text("10.0.0.21 or pdu-200a.example.com"))
                    TextField("Port", value: $device.port, format: .number.grouping(.never))
                    TextField("Read community", text: $device.readCommunity, prompt: Text("public"))
                    TextField("Write community", text: $device.writeCommunity, prompt: Text("same as read if empty"))
                }
                Section("Limits") {
                    TextField("This PDU, A", value: $device.maxAmps, format: .number)
                    TextField("One bank, A", text: $bankLimitText, prompt: Text("optional"))
                }
                Section {
                    HStack {
                        Button(testing ? "Testing…" : "Test connection") { runTest() }.disabled(testing || device.host.isEmpty)
                        if let testResult { Text(testResult).foregroundStyle(testOK ? Color.green : Color.red).font(.callout).fixedSize(horizontal: false, vertical: true) }
                    }
                }
            }
            .formStyle(.grouped)
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }.keyboardShortcut(.cancelAction)
                Button(isNew ? "Add" : "Save") { save() }.keyboardShortcut(.defaultAction).disabled(!valid)
            }
            .padding([.horizontal, .bottom], 20)
        }
        .frame(width: 520, height: 600)
        .onAppear { bankLimitText = device.bankMaxAmps.map { Fmt.limit($0) } ?? "" }
    }

    private var valid: Bool {
        !device.name.trimmingCharacters(in: .whitespaces).isEmpty && !device.host.trimmingCharacters(in: .whitespaces).isEmpty
            && device.port > 0 && device.port < 65536 && device.maxAmps > 0
    }

    private func normalized() -> DeviceConfig {
        var d = device
        d.name = d.name.trimmingCharacters(in: .whitespaces)
        d.host = d.host.trimmingCharacters(in: .whitespaces)
        d.bankMaxAmps = Double(bankLimitText.replacingOccurrences(of: ",", with: "."))
        return d
    }

    private func save() {
        let d = normalized()
        if isNew { model.addDevice(d) } else { model.updateDevice(d) }
        dismiss()
    }

    private func runTest() {
        testing = true; testResult = nil
        let d = normalized()
        Task {
            switch await model.test(d) {
            case .success(let snapshot):
                testOK = true
                testResult = "Connected: \(snapshot.info.model ?? d.vendor.displayName), \(snapshot.info.outletCount) outlets, \(Fmt.amps(snapshot.totalAmps)) A"
            case .failure(let error):
                testOK = false
                testResult = error.localizedDescription
            }
            testing = false
        }
    }
}

// MARK: - General

struct GeneralSettings: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        Form {
            Section("Reading") {
                Stepper(value: $model.settings.pollInterval, in: 3...300, step: 1) { Text("Refresh every \(Int(model.settings.pollInterval)) seconds") }
                Stepper(value: $model.settings.warnPercent, in: 50...99, step: 5) { Text("Warn from \(Int(model.settings.warnPercent))% of a limit") }
                Toggle("Notify when a rack goes over its limit", isOn: $model.settings.notifyWhenOver)
            }
            Section("Restart") {
                Stepper(value: $model.settings.restartDelay, in: 3...60, step: 1) { Text("Ports stay off for \(Int(model.settings.restartDelay)) seconds") }
                Text("A restart switches all ports of the server off, waits for the PDUs to confirm, pauses, then switches them on.").font(.caption).foregroundStyle(.secondary)
            }
            Section("Updates") {
                UpdatesRow()
            }
            Section("Demo") {
                Toggle("Demo mode (sample racks, nothing is sent to real PDUs)", isOn: $model.settings.demoMode)
            }
        }
        .formStyle(.grouped)
    }
}

struct UpdatesRow: View {
    @EnvironmentObject var model: AppModel
    var body: some View {
        HStack {
            Text("Version \(AppVersion.display)")
            Spacer()
            if model.updater.configured {
                Toggle("Check automatically", isOn: Binding(get: { model.updater.automaticChecks }, set: { model.updater.automaticChecks = $0 }))
            }
            Button("Check for updates…") { model.updater.checkNow() }
        }
        if !model.updater.configured {
            Text("Updates are not set up in this build.").font(.caption).foregroundStyle(.secondary)
        }
    }
}
