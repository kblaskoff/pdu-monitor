import AppKit
import Combine
import Sparkle

/// Updates with Sparkle. The feed address and the public key are in Info.plist (SUFeedURL, SUPublicEDKey); a build without the key
/// (a test build) shows "not set up" and never contacts anything. See docs/UPDATES.md.
final class AppUpdater: ObservableObject {
    @Published private(set) var canCheck = false
    @Published private(set) var configured = false
    private var controller: SPUStandardUpdaterController?
    private var observation: NSKeyValueObservation?

    init(bundle: Bundle = .main) {
        // Unit tests and direct swift-run binaries have no installed app bundle.
        guard bundle.bundleURL.pathExtension == "app",
              let key = bundle.object(forInfoDictionaryKey: "SUPublicEDKey") as? String,
              Data(base64Encoded: key)?.count == 32 else { return }
        configured = true
        let controller = SPUStandardUpdaterController(startingUpdater: false, updaterDelegate: nil, userDriverDelegate: nil)
        self.controller = controller
        observation = controller.updater.observe(\.canCheckForUpdates, options: [.initial, .new]) { [weak self] updater, _ in
            DispatchQueue.main.async { self?.canCheck = updater.canCheckForUpdates }
        }
        controller.startUpdater()
    }

    var automaticChecks: Bool {
        get { controller?.updater.automaticallyChecksForUpdates ?? false }
        set { controller?.updater.automaticallyChecksForUpdates = newValue; objectWillChange.send() }
    }

    func checkNow() {
        guard configured else {
            let alert = NSAlert()
            alert.messageText = "Updates are not set up in this build"
            alert.informativeText = "Install a release build to receive updates."
            alert.runModal()
            return
        }
        guard canCheck else { return }
        controller?.checkForUpdates(nil)
    }
}
