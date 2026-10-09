import AppKit

// Draws the application icon (a rack unit with a lightning bolt) into an .iconset folder: swift make-icon.swift output.iconset
guard CommandLine.arguments.count == 2 else { fputs("Usage: swift make-icon.swift output.iconset\n", stderr); exit(1) }
let destination = URL(fileURLWithPath: CommandLine.arguments[1], isDirectory: true)
try FileManager.default.createDirectory(at: destination, withIntermediateDirectories: true)
let variants: [(String, Int)] = [
    ("icon_16x16.png", 16), ("icon_16x16@2x.png", 32), ("icon_32x32.png", 32), ("icon_32x32@2x.png", 64),
    ("icon_128x128.png", 128), ("icon_128x128@2x.png", 256), ("icon_256x256.png", 256),
    ("icon_256x256@2x.png", 512), ("icon_512x512.png", 512), ("icon_512x512@2x.png", 1024)
]

func draw(size s: CGFloat) {
    let tile = NSRect(x: s * 0.05, y: s * 0.05, width: s * 0.90, height: s * 0.90)
    let tilePath = NSBezierPath(roundedRect: tile, xRadius: s * 0.21, yRadius: s * 0.21)
    NSGradient(starting: NSColor(calibratedRed: 0.10, green: 0.16, blue: 0.30, alpha: 1),
               ending: NSColor(calibratedRed: 0.05, green: 0.07, blue: 0.14, alpha: 1))?.draw(in: tilePath, angle: -90)
    // three rack units
    for i in 0..<3 {
        let unit = NSRect(x: s * 0.20, y: s * (0.25 + 0.17 * CGFloat(i)), width: s * 0.60, height: s * 0.12)
        NSColor(calibratedWhite: 1, alpha: 0.14).setFill()
        NSBezierPath(roundedRect: unit, xRadius: s * 0.02, yRadius: s * 0.02).fill()
        let light = NSRect(x: unit.minX + s * 0.03, y: unit.midY - s * 0.014, width: s * 0.028, height: s * 0.028)
        (i == 2 ? NSColor.systemOrange : NSColor.systemGreen).setFill()
        NSBezierPath(ovalIn: light).fill()
    }
    // lightning bolt
    let bolt = NSBezierPath()
    func p(_ x: CGFloat, _ y: CGFloat) -> NSPoint { NSPoint(x: s * x, y: s * y) }
    bolt.move(to: p(0.56, 0.84)); bolt.line(to: p(0.34, 0.50)); bolt.line(to: p(0.48, 0.50))
    bolt.line(to: p(0.42, 0.20)); bolt.line(to: p(0.68, 0.58)); bolt.line(to: p(0.53, 0.58)); bolt.close()
    NSColor(calibratedRed: 1.0, green: 0.80, blue: 0.20, alpha: 1).setFill()
    bolt.fill()
}

for (name, pixels) in variants {
    guard let bitmap = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: pixels, pixelsHigh: pixels, bitsPerSample: 8, samplesPerPixel: 4,
                                        hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0),
          let context = NSGraphicsContext(bitmapImageRep: bitmap) else { exit(1) }
    NSGraphicsContext.saveGraphicsState(); NSGraphicsContext.current = context
    context.imageInterpolation = .high
    draw(size: CGFloat(pixels))
    NSGraphicsContext.restoreGraphicsState()
    guard let data = bitmap.representation(using: .png, properties: [:]) else { exit(1) }
    try data.write(to: destination.appendingPathComponent(name))
}
