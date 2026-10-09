import SwiftUI
import PDUCore

struct OverviewView: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                totals
                if model.settings.overviewAsList { list } else { tiles }
            }
            .padding(20)
        }
        .navigationTitle("Overview")
        .toolbar {
            ToolbarItem {
                Picker("View", selection: $model.settings.overviewAsList) {
                    Image(systemName: "square.grid.2x2").tag(false)
                    Image(systemName: "list.bullet").tag(true)
                }.pickerStyle(.segmented)
            }
            ToolbarItem {
                Toggle(isOn: $model.settings.sortByLoad) { Label("Busiest first", systemImage: "arrow.up.arrow.down") }
                    .help("Racks over their limit first, then by load")
            }
        }
    }

    private var totals: some View {
        HStack(alignment: .top, spacing: 28) {
            stat("Total power", "\(Fmt.kw(model.totalWatts)) kW")
            stat("Total current", "\(Fmt.amps(model.totalAmps)) A")
            stat("Racks", "\(model.racks.count)")
            VStack(alignment: .leading, spacing: 2) {
                Text("Over limit").font(.caption).foregroundStyle(.secondary)
                Text("\(model.racksNeedingAttention)").font(.system(size: 26, weight: .semibold, design: .rounded))
                    .foregroundStyle(model.racksNeedingAttention > 0 ? Color.red : Color.green)
            }
            Spacer()
            if let last = model.lastPoll {
                VStack(alignment: .trailing, spacing: 2) {
                    Text("Updated").font(.caption).foregroundStyle(.secondary)
                    Text(last, style: .time).monospacedDigit()
                }
            }
        }
    }

    private func stat(_ title: String, _ value: String) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title).font(.caption).foregroundStyle(.secondary)
            Text(value).font(.system(size: 26, weight: .semibold, design: .rounded)).monospacedDigit()
        }
    }

    private var tiles: some View {
        LazyVGrid(columns: [GridItem(.adaptive(minimum: 230, maximum: 420), spacing: 14)], spacing: 14) {
            ForEach(model.orderedRacks) { rack in
                Button { model.route = .rack(rack.id) } label: { RackTile(rack: rack, summary: model.summary(of: rack)) }
                    .buttonStyle(.plain)
            }
        }
    }

    private var list: some View {
        Table(model.orderedRacks) {
            TableColumn("Rack") { rack in
                Button(rack.name) { model.route = .rack(rack.id) }.buttonStyle(.link).font(.body.weight(.semibold))
            }.width(min: 70, ideal: 110)
            TableColumn("kW") { rack in Text(Fmt.kw(model.summary(of: rack).watts)).monospacedDigit() }.width(60)
            TableColumn("Amps") { rack in
                let s = model.summary(of: rack)
                Text(Fmt.amps(s.amps)).monospacedDigit().foregroundStyle(s.status.color)
            }.width(70)
            TableColumn("Limit") { rack in Text("\(Fmt.limit(rack.maxAmps)) A").monospacedDigit() }.width(70)
            TableColumn("Load") { rack in
                let s = model.summary(of: rack)
                HStack { LimitBar(fraction: s.fraction, status: s.status, height: 8); Text(Fmt.percent(s.fraction)).monospacedDigit().frame(width: 44, alignment: .trailing) }
            }.width(min: 120, ideal: 200)
            TableColumn("Status") { rack in StatusPill(status: model.summary(of: rack).attention) }.width(min: 100, ideal: 120)
        }
        .frame(height: CGFloat(model.racks.count) * 28 + 70)
    }
}

struct RackTile: View {
    let rack: RackConfig
    let summary: RackSummary

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text(rack.name).font(.title2.weight(.bold))
                Spacer()
                StatusPill(status: summary.attention)
            }
            HStack(alignment: .firstTextBaseline, spacing: 4) {
                Text(Fmt.amps(summary.amps)).font(.system(size: 36, weight: .semibold, design: .rounded)).monospacedDigit()
                Text("A of \(Fmt.limit(rack.maxAmps)) A").foregroundStyle(.secondary)
            }
            LimitBar(fraction: summary.fraction, status: summary.status)
            HStack {
                Text("\(Fmt.kw(summary.watts)) kW").monospacedDigit()
                Spacer()
                if summary.offlineCount > 0 {
                    Label("\(summary.offlineCount) of \(summary.pduCount) PDU offline", systemImage: "wifi.exclamationmark").foregroundStyle(.orange)
                } else {
                    Text("\(summary.pduCount) PDU")
                }
            }
            .font(.callout).foregroundStyle(.secondary)
            if summary.worstPDU == .over || summary.worstPDU == .warning {
                Text(summary.worstPDU == .over ? "A PDU in this rack is over its own limit" : "A PDU in this rack is close to its own limit")
                    .font(.caption).foregroundStyle(summary.worstPDU.color)
            }
        }
        .padding(14)
        .background(RoundedRectangle(cornerRadius: 12).fill(summary.attention == .over ? Color.red.opacity(0.10) : Color(nsColor: .controlBackgroundColor)))
        .overlay(RoundedRectangle(cornerRadius: 12).strokeBorder(summary.attention == .over ? Color.red : Color.secondary.opacity(0.25), lineWidth: summary.attention == .over ? 2 : 1))
        .contentShape(RoundedRectangle(cornerRadius: 12))
    }
}
