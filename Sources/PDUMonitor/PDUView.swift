import SwiftUI
import PDUCore

struct PDUView: View {
    @EnvironmentObject var model: AppModel
    let pduID: UUID
    @State private var selection: Set<Int> = []

    var body: some View {
        if let state = model.states[pduID] ?? model.device(pduID).map({ PDUState(config: $0) }) {
            let config = state.config
            let outlets = state.snapshot?.outlets ?? []
            let status = state.status(warnFraction: model.settings.warnFraction)
            VStack(alignment: .leading, spacing: 14) {
                header(state, status)
                HStack(alignment: .top, spacing: 16) {
                    VStack(alignment: .leading, spacing: 10) {
                        outletTable(outlets, dimmed: state.error != nil)
                        PowerActionBar(noun: "port", selectedCount: selection.count,
                                       targets: { selection.sorted().map { OutletTarget(pduID: pduID, outlet: $0) } },
                                       selectAll: { selection = Set(outlets.map(\.number)) }, clear: { selection = [] })
                        totals(state)
                    }
                    infoPanel(state)
                }
            }
            .padding(20)
            .navigationTitle("PDU \(config.name)")
            .onChange(of: outlets.map(\.number)) { _, numbers in selection = selection.intersection(numbers) }
        } else {
            ContentUnavailableView("PDU not found", systemImage: "questionmark.folder")
        }
    }

    private func header(_ state: PDUState, _ status: LimitStatus) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .firstTextBaseline, spacing: 12) {
                Text(state.config.name).font(.largeTitle.weight(.bold))
                Text(state.config.vendor.displayName).foregroundStyle(.secondary)
                StatusPill(status: status)
                Spacer()
                HStack(alignment: .firstTextBaseline, spacing: 4) {
                    Text(Fmt.amps(state.amps)).font(.system(size: 30, weight: .semibold, design: .rounded)).monospacedDigit()
                        .foregroundStyle(status == .unknown ? Color.primary : status.color)
                    Text("A of \(Fmt.limit(state.config.maxAmps)) A  ·  \(Fmt.watts(state.watts)) W").foregroundStyle(.secondary).monospacedDigit()
                }
            }
            LimitBar(fraction: state.amps.map { $0 / max(state.config.maxAmps, 0.1) }, status: status)
            if let error = state.error {
                Label(state.snapshot == nil ? error : "\(error) — showing the last reading", systemImage: "wifi.exclamationmark")
                    .font(.callout).foregroundStyle(.orange)
            }
        }
    }

    private func outletTable(_ outlets: [OutletReading], dimmed: Bool) -> some View {
        Table(outlets) {
            TableColumn("") { outlet in Toggle("", isOn: selectionBinding($selection, outlet.number)).labelsHidden() }.width(26)
            TableColumn("Outlet") { outlet in Text("\(outlet.number)").monospacedDigit() }.width(52)
            TableColumn("Server") { outlet in
                Text(outlet.isAssigned ? outlet.name : "—").foregroundStyle(outlet.isAssigned ? Color.primary : Color.secondary)
                    .font(outlet.isAssigned ? .body.weight(.semibold) : .body)
            }.width(min: 90, ideal: 130)
            TableColumn("Bank") { outlet in Text(outlet.bank.map(String.init) ?? "—").foregroundStyle(.secondary) }.width(44)
            TableColumn("State") { outlet in PowerDot(isOn: outlet.isOn) }.width(70)
            TableColumn("Current (A)") { outlet in
                Text(Fmt.amps(outlet.amps)).monospacedDigit().foregroundStyle(outlet.amps == nil || outlet.amps == 0 ? Color.secondary : Color.primary)
            }.width(86)
            TableColumn("Power (W)") { outlet in
                Text(Fmt.watts(outlet.watts)).monospacedDigit().foregroundStyle(outlet.watts == nil || outlet.watts == 0 ? Color.secondary : Color.primary)
            }.width(86)
        }
        .opacity(dimmed ? 0.55 : 1)
        .frame(minHeight: 260)
    }

    private func totals(_ state: PDUState) -> some View {
        let warn = model.settings.warnFraction
        return VStack(alignment: .leading, spacing: 4) {
            if let snapshot = state.snapshot {
                ForEach(snapshot.phases) { phase in
                    totalRow("Total phase \(phase.number):", amps: phase.amps, watts: phase.watts,
                             status: snapshot.phases.count == 1 ? state.status(warnFraction: warn) : .unknown)
                }
                ForEach(snapshot.banks) { bank in
                    totalRow("Total bank \(bank.number):", amps: bank.amps, watts: bank.watts,
                             status: LimitStatus.evaluate(amps: bank.amps, limit: state.config.bankMaxAmps, warnFraction: warn))
                }
            }
        }
        .font(.callout)
    }

    private func totalRow(_ title: String, amps: Double, watts: Double?, status: LimitStatus) -> some View {
        HStack {
            Text(title).foregroundStyle(.secondary).frame(width: 130, alignment: .trailing)
            Text("\(Fmt.amps(amps)) A").monospacedDigit().foregroundStyle(status == .unknown ? Color.primary : status.color).frame(width: 70, alignment: .trailing)
            Text(watts.map { "\(Fmt.watts($0)) W" } ?? "—").monospacedDigit().foregroundStyle(.secondary).frame(width: 80, alignment: .trailing)
        }
    }

    private func infoPanel(_ state: PDUState) -> some View {
        let info = state.snapshot?.info
        return VStack(alignment: .leading, spacing: 6) {
            Text("Device").font(.headline)
            InfoRow(title: "Vendor", value: state.config.vendor.displayName)
            InfoRow(title: "Model", value: info?.model ?? "—")
            InfoRow(title: "Outlets", value: info.map { String($0.outletCount) } ?? "—")
            InfoRow(title: "Breakers", value: info?.breakerCount.map(String.init) ?? "—")
            InfoRow(title: "Orientation", value: info?.orientation ?? "—")
            InfoRow(title: "Voltage", value: info?.lineVoltage.map { "\(Fmt.limit($0)) V" } ?? "—")
            InfoRow(title: "Firmware", value: info?.firmware ?? "—")
            InfoRow(title: "Serial", value: info?.serial ?? "—")
            InfoRow(title: "Address", value: "\(state.config.host):\(state.config.port)")
            InfoRow(title: "PDU limit", value: "\(Fmt.limit(state.config.maxAmps)) A")
            if state.snapshot != nil, state.snapshot?.hasOutletMetering == false {
                Text("This PDU does not measure single outlets, only banks and the whole unit.").font(.caption).foregroundStyle(.secondary)
                    .padding(.leading, 4).padding(.top, 4)
            }
        }
        .padding(14)
        .frame(width: 300, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 10).fill(Color(nsColor: .controlBackgroundColor)))
        .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(Color.secondary.opacity(0.25)))
    }
}
