import SwiftUI
import PDUCore

enum Fmt {
    static func amps(_ v: Double?) -> String { v.map { String(format: "%.1f", $0) } ?? "—" }
    static func amps2(_ v: Double?) -> String { v.map { String(format: "%.2f", $0) } ?? "—" }
    static func watts(_ v: Double?) -> String { v.map { String(format: "%.0f", $0) } ?? "—" }
    static func kw(_ v: Double?) -> String { v.map { String(format: "%.2f", $0 / 1000) } ?? "—" }
    static func limit(_ v: Double) -> String { v == v.rounded() ? String(format: "%.0f", v) : String(format: "%.1f", v) }
    static func percent(_ v: Double?) -> String { v.map { String(format: "%.0f%%", $0 * 100) } ?? "—" }
}

extension LimitStatus {
    var color: Color {
        switch self {
        case .ok: return .green
        case .warning: return .orange
        case .over: return .red
        case .unknown: return .secondary
        }
    }
    var title: String {
        switch self {
        case .ok: return "OK"
        case .warning: return "High"
        case .over: return "Over limit"
        case .unknown: return "No data"
        }
    }
    var symbol: String {
        switch self {
        case .ok: return "checkmark.circle.fill"
        case .warning: return "exclamationmark.triangle.fill"
        case .over: return "bolt.trianglebadge.exclamationmark.fill"
        case .unknown: return "questionmark.circle"
        }
    }
}

struct StatusPill: View {
    let status: LimitStatus
    var body: some View {
        Label(status.title, systemImage: status.symbol)
            .font(.caption.weight(.semibold))
            .foregroundStyle(status.color)
            .padding(.horizontal, 8).padding(.vertical, 3)
            .background(Capsule().fill(status.color.opacity(0.14)))
    }
}

/// A horizontal bar for "how much of the limit is used". A tick shows where the limit is when the bar runs past it.
struct LimitBar: View {
    let fraction: Double?
    let status: LimitStatus
    var height: CGFloat = 10
    var body: some View {
        GeometryReader { proxy in
            let shown = min(max(fraction ?? 0, 0), 1)
            ZStack(alignment: .leading) {
                Capsule().fill(Color.secondary.opacity(0.18))
                Capsule().fill(status.color).frame(width: max(fraction == nil ? 0 : height, proxy.size.width * shown))
            }
        }
        .frame(height: height)
        .accessibilityLabel("Load")
        .accessibilityValue(Fmt.percent(fraction))
    }
}

struct PowerDot: View {
    let isOn: Bool?
    var body: some View {
        HStack(spacing: 5) {
            Circle().fill(color).frame(width: 8, height: 8)
            Text(label).foregroundStyle(isOn == false ? Color.red : Color.primary)
        }
    }
    private var color: Color { isOn == nil ? .secondary : (isOn! ? .green : .red) }
    private var label: String { isOn == nil ? "?" : (isOn! ? "ON" : "OFF") }
}

extension ServerPower {
    var dotValue: Bool? {
        switch self { case .on: return true; case .off: return false; default: return nil }
    }
}

/// The checkbox column: a toggle that adds and removes an id from a selection.
func selectionBinding<ID: Hashable>(_ selection: Binding<Set<ID>>, _ id: ID) -> Binding<Bool> {
    Binding(get: { selection.wrappedValue.contains(id) },
            set: { on in if on { selection.wrappedValue.insert(id) } else { selection.wrappedValue.remove(id) } })
}

struct PendingAction: Identifiable {
    let id = UUID()
    var operation: PowerOperation
    var verb: String
    var title: String
    var message: String
    var targets: [OutletTarget]
}

/// "Restart / On / Off" for what is ticked, with the confirmation pop-up. Nothing is switched without it.
struct PowerActionBar: View {
    @EnvironmentObject var model: AppModel
    let noun: String                     // "server" or "port"
    let selectedCount: Int
    let targets: () -> [OutletTarget]
    let selectAll: () -> Void
    let clear: () -> Void
    @State private var pending: PendingAction?

    var body: some View {
        HStack(spacing: 10) {
            Button("Select all", action: selectAll)
            Button("Clear", action: clear).disabled(selectedCount == 0)
            Text(selectedCount == 0 ? "Tick \(noun)s to switch them" : "\(selectedCount) \(noun)\(selectedCount == 1 ? "" : "s") selected")
                .foregroundStyle(.secondary)
            Spacer()
            Button { ask(.restart(delay: model.settings.restartDelay), verb: "Restart") } label: { Label("Restart", systemImage: "arrow.clockwise") }
            Button { ask(.on, verb: "Switch on") } label: { Label("On", systemImage: "power") }
            Button { ask(.off, verb: "Switch off") } label: { Label("Off", systemImage: "poweroff") }
        }
        .disabled(model.busy)
        .controlSize(.regular)
        .alert(pending?.title ?? "", isPresented: Binding(get: { pending != nil }, set: { if !$0 { pending = nil } }), presenting: pending) { action in
            Button("Cancel", role: .cancel) {}
            Button(action.verb, role: .destructive) { model.perform(action.operation, targets: action.targets, title: action.title) }
        } message: { action in
            Text(action.message)
        }
    }

    private func ask(_ operation: PowerOperation, verb: String) {
        let chosen = targets()
        guard !chosen.isEmpty else { return }
        let lines = model.describe(chosen)
        let shown = lines.prefix(10).joined(separator: "\n") + (lines.count > 10 ? "\n… and \(lines.count - 10) more" : "")
        let what = "\(selectedCount) \(noun)\(selectedCount == 1 ? "" : "s") (\(chosen.count) port\(chosen.count == 1 ? "" : "s"))"
        let effect: String
        switch operation {
        case .restart(let delay): effect = "All ports are switched off, the PDUs confirm it, then after \(Int(delay)) seconds all are switched on again. The power is lost during that time."
        case .off: effect = "The power is cut at once."
        case .on: effect = "The power is restored at once."
        }
        pending = PendingAction(operation: operation, verb: verb, title: "\(verb) \(what)?", message: "\(shown)\n\n\(effect)", targets: chosen)
    }
}

/// The strip at the bottom of the window while ports are being switched (and after a failure, until it is closed).
struct OperationBanner: View {
    @EnvironmentObject var model: AppModel
    var body: some View {
        if let op = model.operation {
            HStack(spacing: 10) {
                if op.running { ProgressView().controlSize(.small) }
                else if op.error != nil { Image(systemName: "exclamationmark.octagon.fill").foregroundStyle(.red) }
                else { Image(systemName: "checkmark.circle.fill").foregroundStyle(.green) }
                VStack(alignment: .leading, spacing: 2) {
                    Text(op.title).font(.callout.weight(.semibold))
                    Text(op.error ?? op.detail).font(.caption).foregroundStyle(op.error == nil ? Color.secondary : Color.red)
                        .fixedSize(horizontal: false, vertical: true)
                }
                Spacer()
                if !op.running { Button("Close") { model.operation = nil } }
            }
            .padding(12)
            .background(.regularMaterial)
            .overlay(alignment: .top) { Divider() }
        }
    }
}

struct InfoRow: View {
    let title: String, value: String
    var body: some View {
        HStack(alignment: .firstTextBaseline) {
            Text(title).foregroundStyle(.secondary).frame(width: 84, alignment: .trailing)
            Text(value).textSelection(.enabled)
            Spacer(minLength: 0)
        }
        .font(.callout)
    }
}
