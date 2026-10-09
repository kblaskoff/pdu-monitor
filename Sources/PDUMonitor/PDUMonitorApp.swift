import SwiftUI

@main
struct PDUMonitorApp: App {
    @StateObject private var model = AppModel()

    var body: some Scene {
        WindowGroup("PDU Monitor") {
            ContentView().environmentObject(model)
        }
        .defaultSize(width: 1180, height: 760)
        .windowResizability(.contentMinSize)
        .commands {
            CommandGroup(after: .appInfo) {
                Button("Check for Updates…") { model.updater.checkNow() }
            }
        }
        Settings {
            SettingsView().environmentObject(model)
        }
    }
}
