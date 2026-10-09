import SwiftUI
import PDUCore

struct RackView: View {
    @EnvironmentObject var model: AppModel
    let rackID: UUID
    @State private var selection: Set<String> = []
    @State private var renaming: ServerEntry?

    var body: some View {
        if let rack = model.rack(rackID) {
            let summary = model.summary(of: rack)
            let servers = model.servers(in: rackID)
            VStack(alignment: .leading, spacing: 14) {
                header(rack, summary)
                pduStrip
                serverTable(servers)
                PowerActionBar(noun: "server", selectedCount: selection.count,
                               targets: { servers.filter { selection.contains($0.key) }.flatMap(\.targets) },
                               selectAll: { selection = Set(servers.map(\.key)) }, clear: { selection = [] })
                footer(summary)
            }
            .padding(20)
            .frame(maxHeight: .infinity, alignment: .top)
            .navigationTitle("Rack \(rack.name)")
            .sheet(item: $renaming) { server in
                RenameServerSheet(server: server, rackID: rackID)
            }
            .onChange(of: servers.map(\.key)) { _, keys in selection = selection.intersection(keys) }
        } else {
            ContentUnavailableView("Rack not found", systemImage: "questionmark.folder")
        }
    }

    private func header(_ rack: RackConfig, _ summary: RackSummary) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .firstTextBaseline, spacing: 14) {
                Text("Rack \(rack.name)").font(.largeTitle.weight(.bold))
                StatusPill(status: summary.attention)
                Spacer()
                VStack(alignment: .trailing, spacing: 0) {
                    HStack(alignment: .firstTextBaseline, spacing: 4) {
                        Text(Fmt.amps(summary.amps)).font(.system(size: 34, weight: .semibold, design: .rounded)).monospacedDigit()
                            .foregroundStyle(summary.status == .unknown ? Color.primary : summary.status.color)
                        Text("A of \(Fmt.limit(rack.maxAmps)) A").foregroundStyle(.secondary)
                    }
                    Text("\(Fmt.kw(summary.watts)) kW  ·  \(Fmt.watts(summary.watts)) W").foregroundStyle(.secondary).monospacedDigit()
                }
            }
            LimitBar(fraction: summary.fraction, status: summary.status, height: 12)
        }
    }

    /// The PDUs of the rack with their own load against their own limit.
    private var pduStrip: some View {
        LazyVGrid(columns: [GridItem(.adaptive(minimum: 250, maximum: 460), spacing: 12, alignment: .top)], alignment: .leading, spacing: 12) {
            ForEach(model.pduStates(in: rackID)) { pdu in
                let status = pdu.status(warnFraction: model.settings.warnFraction)
                Button { model.route = .pdu(pdu.id) } label: {
                    VStack(alignment: .leading, spacing: 4) {
                        HStack {
                            Text(pdu.config.name).font(.headline)
                            Text(pdu.config.vendor.displayName).font(.caption).foregroundStyle(.secondary)
                            Spacer()
                            if pdu.error != nil { Image(systemName: "wifi.exclamationmark").foregroundStyle(.orange).help(pdu.error ?? "") }
                        }
                        HStack(alignment: .firstTextBaseline, spacing: 4) {
                            Text(Fmt.amps(pdu.amps)).font(.title3.weight(.semibold)).monospacedDigit().foregroundStyle(status == .unknown ? Color.primary : status.color)
                            Text("A of \(Fmt.limit(pdu.config.maxAmps)) A · \(Fmt.watts(pdu.watts)) W").font(.callout).foregroundStyle(.secondary)
                        }
                        LimitBar(fraction: pdu.amps.map { $0 / max(pdu.config.maxAmps, 0.1) }, status: status, height: 6)
                    }
                    .padding(10)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(RoundedRectangle(cornerRadius: 10).fill(Color(nsColor: .controlBackgroundColor)))
                    .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(status == .over ? Color.red : Color.secondary.opacity(0.25), lineWidth: status == .over ? 2 : 1))
                }
                .buttonStyle(.plain)
            }
        }
    }

    private func serverTable(_ servers: [ServerEntry]) -> some View {
        Table(servers) {
            TableColumn("") { server in Toggle("", isOn: selectionBinding($selection, server.key)).labelsHidden() }.width(min: 26, ideal: 26, max: 26)
            TableColumn("Server") { server in
                VStack(alignment: .leading, spacing: 0) {
                    Text(server.name).font(.body.weight(.semibold))
                    if let label = model.label(of: server, rack: rackID) { Text(label).font(.caption).foregroundStyle(.secondary) }
                }
                .contextMenu { Button("Rename…") { renaming = server } }
            }.width(min: 80, ideal: 110, max: 140)
            TableColumn("Type") { server in
                Text(server.kind.displayName).foregroundStyle(server.kind == .network ? Color.yellow : Color.green)
            }.width(min: 70, ideal: 70, max: 70)
            TableColumn("Ports") { server in
                Text(server.ports.map { "\($0.pduName) #\($0.target.outlet)" }.joined(separator: " · ")).foregroundStyle(.secondary)
            }.width(min: 150, ideal: 200, max: 340)
            TableColumn("State") { server in
                HStack(spacing: 5) {
                    PowerDot(isOn: server.power.dotValue)
                    if server.power == .mixed { Text("partly").font(.caption).foregroundStyle(.orange) }
                }
            }.width(min: 80, ideal: 80, max: 80)
            TableColumn("Power (W)") { server in Text(Fmt.watts(server.watts)).monospacedDigit().foregroundStyle(server.watts == 0 ? Color.secondary : Color.primary) }.width(min: 76, ideal: 76, max: 76)
            TableColumn("Current (A)") { server in Text(Fmt.amps2(server.amps)).monospacedDigit().foregroundStyle(server.amps == 0 ? Color.secondary : Color.primary) }.width(min: 84, ideal: 84, max: 84)
            TableColumn("") { _ in EmptyView() }
        }
        .frame(minHeight: 180, maxHeight: .infinity)
        .overlay {
            if servers.isEmpty {
                ContentUnavailableView("No servers yet", systemImage: "server.rack",
                                       description: Text("Name the outlets on the PDUs after the servers (for example 200U31). Outlets with the default name \"Outlet_N\" are not shown."))
            }
        }
    }

    private func footer(_ summary: RackSummary) -> some View {
        HStack {
            Text("Total rack power:").foregroundStyle(.secondary)
            Text("\(Fmt.watts(summary.watts)) W").monospacedDigit().font(.body.weight(.semibold))
            Text("\(Fmt.amps(summary.amps)) A").monospacedDigit().font(.body.weight(.semibold)).foregroundStyle(summary.status.color)
            Spacer()
            Text("Right-click a server to give it a name").font(.caption).foregroundStyle(.tertiary)
        }
    }
}

struct RenameServerSheet: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let server: ServerEntry
    let rackID: UUID
    @State private var text = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Name for \(server.name)").font(.headline)
            Text("Shown under the id in the rack view. The id on the PDU outlets does not change.").font(.callout).foregroundStyle(.secondary)
            TextField("For example: web-01", text: $text).textFieldStyle(.roundedBorder).onSubmit(save)
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }.keyboardShortcut(.cancelAction)
                Button("Save", action: save).keyboardShortcut(.defaultAction)
            }
        }
        .padding(20).frame(width: 380)
        .onAppear { text = model.label(of: server, rack: rackID) ?? "" }
    }

    private func save() { model.setLabel(text, rack: rackID, server: server); dismiss() }
}
