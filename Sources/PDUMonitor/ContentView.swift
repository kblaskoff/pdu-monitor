import SwiftUI
import PDUCore

struct ContentView: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        NavigationSplitView {
            Sidebar()
                .navigationSplitViewColumnWidth(min: 190, ideal: 220, max: 300)
        } detail: {
            Group {
                if model.racks.isEmpty {
                    WelcomeView()
                } else {
                    switch model.route {
                    case .overview: OverviewView()
                    case .rack(let id): RackView(rackID: id)
                    case .pdu(let id): PDUView(pduID: id)
                    }
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            .safeAreaInset(edge: .bottom, spacing: 0) { OperationBanner() }
        }
        .toolbar {
            ToolbarItem {
                Button { Task { await model.poll() } } label: { Label("Refresh", systemImage: "arrow.clockwise") }
                    .disabled(model.polling).help("Read all PDUs now")
            }
            ToolbarItem {
                SettingsLink { Label("Racks & PDUs", systemImage: "gearshape") }.help("Add racks and PDUs, change limits")
            }
        }
        .frame(minWidth: 900, minHeight: 600)
    }
}

struct Sidebar: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        List(selection: Binding(get: { Optional(model.route) }, set: { model.route = $0 ?? .overview })) {
            Label("Overview", systemImage: "square.grid.2x2").tag(Route.overview)
            Section("Racks") {
                ForEach(model.racks) { rack in
                    let summary = model.summary(of: rack)
                    HStack {
                        Circle().fill(summary.attention.color).frame(width: 8, height: 8)
                        Text(rack.name)
                        Spacer()
                        Text(summary.amps.map { "\(Fmt.amps($0)) A" } ?? "—").font(.caption).foregroundStyle(.secondary).monospacedDigit()
                    }
                    .tag(Route.rack(rack.id))
                    ForEach(model.devices(in: rack.id)) { device in
                        let status = model.states[device.id]?.status(warnFraction: model.settings.warnFraction) ?? .unknown
                        HStack {
                            Image(systemName: model.states[device.id]?.error != nil ? "wifi.exclamationmark" : "powerplug").foregroundStyle(status.color)
                            Text(device.name)
                            Spacer()
                            Text(model.states[device.id]?.amps.map { "\(Fmt.amps($0)) A" } ?? "—").font(.caption).foregroundStyle(.secondary).monospacedDigit()
                        }
                        .padding(.leading, 14).font(.callout)
                        .tag(Route.pdu(device.id))
                    }
                }
            }
        }
        .safeAreaInset(edge: .bottom) {
            if model.settings.demoMode {
                Label("Demo mode", systemImage: "play.rectangle.fill").font(.caption).foregroundStyle(.orange).padding(8)
            }
        }
    }
}

struct WelcomeView: View {
    @EnvironmentObject var model: AppModel
    var body: some View {
        VStack(spacing: 18) {
            Image(systemName: "server.rack").font(.system(size: 54)).foregroundStyle(.secondary)
            Text("PDU Monitor").font(.largeTitle.weight(.bold))
            Text("Watch the power of your racks and switch servers on, off and restart them.\nStart with a rack, then add its PDUs.")
                .multilineTextAlignment(.center).foregroundStyle(.secondary)
            HStack {
                Button("Add a rack") { model.addRack(RackConfig(name: "Rack 1")) }.buttonStyle(.borderedProminent)
                Button("Try the demo") { model.settings.demoMode = true }
            }
        }
        .padding(40)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}
