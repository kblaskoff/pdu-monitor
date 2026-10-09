// swift-tools-version: 5.9
import PackageDescription

var products: [Product] = []
#if os(macOS)
products.append(.executable(name: "PDUMonitor", targets: ["PDUMonitor"]))
#endif
var dependencies: [Package.Dependency] = []
var targets: [Target] = [
    // Everything that is not user interface: SNMP v1, PDU drivers, rack arithmetic, the restart sequence.
    // It uses only Foundation and POSIX sockets, so it builds and is tested on Linux as well as on macOS.
    .target(name: "PDUCore"),
    .testTarget(name: "PDUCoreTests", dependencies: ["PDUCore"])
]
#if os(macOS)
// The application itself (SwiftUI, Sparkle) only exists on macOS; Linux builds and tests the core alone.
dependencies.append(.package(url: "https://github.com/sparkle-project/Sparkle", exact: "2.10.0"))
targets.append(.executableTarget(name: "PDUMonitor", dependencies: ["PDUCore", .product(name: "Sparkle", package: "Sparkle")]))
#endif

let package = Package(
    name: "PDUMonitor",
    platforms: [.macOS("14.0")],
    products: products,
    dependencies: dependencies,
    targets: targets,
    swiftLanguageVersions: [.v5]
)
