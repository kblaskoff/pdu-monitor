import Foundation

/// An object identifier such as 1.3.6.1.4.1.3808.1.1.6.
public struct OID: Hashable, Comparable, CustomStringConvertible, Sendable {
    public let components: [UInt32]
    public init(_ components: [UInt32]) { self.components = components }
    /// Parses "1.3.6.1.2.1" (a leading dot is accepted).
    public init?(_ string: String) {
        let parts = string.split(separator: ".", omittingEmptySubsequences: true)
        guard !parts.isEmpty else { return nil }
        var result: [UInt32] = []
        for part in parts {
            guard let value = UInt32(part) else { return nil }
            result.append(value)
        }
        components = result
    }
    public var description: String { components.map(String.init).joined(separator: ".") }
    public func appending(_ more: UInt32...) -> OID { OID(components + more) }
    public func appending(_ index: Int) -> OID { OID(components + [UInt32(index)]) }
    public func hasPrefix(_ prefix: OID) -> Bool {
        components.count >= prefix.components.count && Array(components.prefix(prefix.components.count)) == prefix.components
    }
    public static func < (a: OID, b: OID) -> Bool { a.components.lexicographicallyPrecedes(b.components) }
}

public enum SNMPValue: Equatable, Sendable {
    case integer(Int64)
    /// Gauge32, Counter32, TimeTicks, Counter64 and Unsigned32 are all unsigned numbers for this application.
    case unsigned(UInt64)
    case octetString([UInt8])
    case null
    case oid(OID)
    case ipAddress([UInt8])
    case noSuchObject, noSuchInstance, endOfMibView

    public var intValue: Int? {
        switch self {
        case .integer(let v): return Int(exactly: v)
        case .unsigned(let v): return Int(exactly: v)
        default: return nil
        }
    }
    public var doubleValue: Double? { intValue.map(Double.init) }
    public var stringValue: String? {
        guard case .octetString(let bytes) = self else { return nil }
        let text = String(decoding: bytes, as: UTF8.self)
        return text.trimmingCharacters(in: CharacterSet(charactersIn: "\0 \t\r\n"))
    }
    /// True for the answers that mean "this object does not exist" (SNMPv2 style).
    public var isMissing: Bool {
        switch self { case .noSuchObject, .noSuchInstance, .endOfMibView: return true; default: return false }
    }
}

public struct VarBind: Equatable, Sendable {
    public var oid: OID
    public var value: SNMPValue
    public init(_ oid: OID, _ value: SNMPValue = .null) { self.oid = oid; self.value = value }
}

public enum PDUKind: UInt8, Sendable {
    case get = 0xA0, getNext = 0xA1, response = 0xA2, set = 0xA3
}

public enum BERError: Error, Equatable { case truncated, malformed(String) }

public struct SNMPMessage: Equatable, Sendable {
    public var community: String
    public var kind: PDUKind
    public var requestID: Int32
    public var errorStatus: Int
    public var errorIndex: Int
    public var varbinds: [VarBind]
    public init(community: String, kind: PDUKind, requestID: Int32, errorStatus: Int = 0, errorIndex: Int = 0, varbinds: [VarBind]) {
        self.community = community; self.kind = kind; self.requestID = requestID
        self.errorStatus = errorStatus; self.errorIndex = errorIndex; self.varbinds = varbinds
    }
}

// MARK: - Encoding

enum BEREncoder {
    static func length(_ n: Int) -> [UInt8] {
        if n < 0x80 { return [UInt8(n)] }
        var bytes: [UInt8] = []
        var rest = n
        while rest > 0 { bytes.insert(UInt8(rest & 0xFF), at: 0); rest >>= 8 }
        return [0x80 | UInt8(bytes.count)] + bytes
    }
    static func tlv(_ tag: UInt8, _ content: [UInt8]) -> [UInt8] { [tag] + length(content.count) + content }

    /// Minimal two's complement.
    static func integer(_ value: Int64) -> [UInt8] {
        var bytes: [UInt8] = []
        var v = value
        repeat {
            bytes.insert(UInt8(truncatingIfNeeded: v), at: 0)
            v >>= 8
        } while !((v == 0 && bytes[0] & 0x80 == 0) || (v == -1 && bytes[0] & 0x80 != 0))
        return tlv(0x02, bytes)
    }
    static func unsigned(tag: UInt8, _ value: UInt64) -> [UInt8] {
        var bytes: [UInt8] = []
        var v = value
        repeat { bytes.insert(UInt8(v & 0xFF), at: 0); v >>= 8 } while v > 0
        if bytes[0] & 0x80 != 0 { bytes.insert(0, at: 0) }
        return tlv(tag, bytes)
    }
    static func oid(_ oid: OID) -> [UInt8] {
        let c = oid.components
        var bytes: [UInt8] = []
        if c.count >= 2 { bytes.append(UInt8(c[0] * 40 + c[1])) } else if c.count == 1 { bytes.append(UInt8(c[0] * 40)) }
        for sub in c.dropFirst(2) {
            var chunk: [UInt8] = [UInt8(sub & 0x7F)]
            var rest = sub >> 7
            while rest > 0 { chunk.insert(UInt8(rest & 0x7F) | 0x80, at: 0); rest >>= 7 }
            bytes += chunk
        }
        return tlv(0x06, bytes)
    }
    static func value(_ v: SNMPValue) -> [UInt8] {
        switch v {
        case .integer(let n): return integer(n)
        case .unsigned(let n): return unsigned(tag: 0x42, n)          // Gauge32
        case .octetString(let b): return tlv(0x04, b)
        case .null: return [0x05, 0x00]
        case .oid(let o): return oid(o)
        case .ipAddress(let b): return tlv(0x40, b)
        case .noSuchObject: return [0x80, 0x00]
        case .noSuchInstance: return [0x81, 0x00]
        case .endOfMibView: return [0x82, 0x00]
        }
    }
}

extension SNMPMessage {
    public func encode() -> [UInt8] {
        let binds = varbinds.flatMap { BEREncoder.tlv(0x30, BEREncoder.oid($0.oid) + BEREncoder.value($0.value)) }
        let pdu = BEREncoder.integer(Int64(requestID)) + BEREncoder.integer(Int64(errorStatus)) + BEREncoder.integer(Int64(errorIndex))
            + BEREncoder.tlv(0x30, binds)
        let body = BEREncoder.integer(0) + BEREncoder.tlv(0x04, Array(community.utf8)) + BEREncoder.tlv(kind.rawValue, pdu)
        return BEREncoder.tlv(0x30, body)
    }
}

// MARK: - Decoding

struct BERReader {
    let bytes: [UInt8]
    var position = 0
    init(_ bytes: [UInt8]) { self.bytes = bytes }
    var isAtEnd: Bool { position >= bytes.count }

    mutating func readByte() throws -> UInt8 {
        guard position < bytes.count else { throw BERError.truncated }
        defer { position += 1 }
        return bytes[position]
    }
    mutating func readLength() throws -> Int {
        let first = try readByte()
        if first < 0x80 { return Int(first) }
        let count = Int(first & 0x7F)
        guard count > 0, count <= 4 else { throw BERError.malformed("length") }
        var n = 0
        for _ in 0..<count { n = (n << 8) | Int(try readByte()) }
        return n
    }
    /// Reads one element: its tag and its content bytes.
    mutating func readTLV() throws -> (tag: UInt8, content: [UInt8]) {
        let tag = try readByte()
        let length = try readLength()
        guard length >= 0, position + length <= bytes.count else { throw BERError.truncated }
        defer { position += length }
        return (tag, Array(bytes[position..<position + length]))
    }
}

enum BERDecoder {
    static func integer(_ content: [UInt8]) throws -> Int64 {
        guard !content.isEmpty, content.count <= 8 else { throw BERError.malformed("integer") }
        var v: Int64 = content[0] & 0x80 != 0 ? -1 : 0
        for b in content { v = (v << 8) | Int64(b) }
        return v
    }
    static func unsigned(_ content: [UInt8]) throws -> UInt64 {
        guard !content.isEmpty else { throw BERError.malformed("unsigned") }
        var bytes = content[...]
        while bytes.count > 1, bytes.first == 0 { bytes = bytes.dropFirst() }
        guard bytes.count <= 8 else { throw BERError.malformed("unsigned") }
        var v: UInt64 = 0
        for b in bytes { v = (v << 8) | UInt64(b) }
        return v
    }
    static func oid(_ content: [UInt8]) throws -> OID {
        guard let first = content.first else { throw BERError.malformed("oid") }
        var parts: [UInt32] = first < 80 ? [UInt32(first / 40), UInt32(first % 40)] : [2, UInt32(first) - 80]
        var current: UInt32 = 0
        var pending = false
        for b in content.dropFirst() {
            guard current <= (UInt32.max >> 7) else { throw BERError.malformed("oid") }
            current = (current << 7) | UInt32(b & 0x7F)
            if b & 0x80 == 0 { parts.append(current); current = 0; pending = false } else { pending = true }
        }
        guard !pending else { throw BERError.malformed("oid") }
        return OID(parts)
    }
    static func value(tag: UInt8, content: [UInt8]) throws -> SNMPValue {
        switch tag {
        case 0x02: return .integer(try integer(content))
        case 0x04: return .octetString(content)
        case 0x05: return .null
        case 0x06: return .oid(try oid(content))
        case 0x40: return .ipAddress(content)
        case 0x41, 0x42, 0x43, 0x46: return .unsigned(try unsigned(content))    // Counter32, Gauge32, TimeTicks, Counter64
        case 0x44: return .octetString(content)                                // Opaque
        case 0x80: return .noSuchObject
        case 0x81: return .noSuchInstance
        case 0x82: return .endOfMibView
        default: throw BERError.malformed("value tag \(tag)")
        }
    }
}

extension SNMPMessage {
    public static func decode(_ data: [UInt8]) throws -> SNMPMessage {
        var outer = BERReader(data)
        let (messageTag, messageBody) = try outer.readTLV()
        guard messageTag == 0x30 else { throw BERError.malformed("message") }
        var reader = BERReader(messageBody)
        let version = try reader.readTLV()
        guard version.tag == 0x02, try BERDecoder.integer(version.content) == 0 else { throw BERError.malformed("version") }
        let community = try reader.readTLV()
        guard community.tag == 0x04 else { throw BERError.malformed("community") }
        let pdu = try reader.readTLV()
        guard let kind = PDUKind(rawValue: pdu.tag) else { throw BERError.malformed("pdu tag \(pdu.tag)") }
        var p = BERReader(pdu.content)
        let id = try p.readTLV(), status = try p.readTLV(), index = try p.readTLV(), list = try p.readTLV()
        guard id.tag == 0x02, status.tag == 0x02, index.tag == 0x02, list.tag == 0x30 else { throw BERError.malformed("pdu") }
        var binds: [VarBind] = []
        var l = BERReader(list.content)
        while !l.isAtEnd {
            let entry = try l.readTLV()
            guard entry.tag == 0x30 else { throw BERError.malformed("varbind") }
            var e = BERReader(entry.content)
            let name = try e.readTLV(), val = try e.readTLV()
            guard name.tag == 0x06 else { throw BERError.malformed("varbind name") }
            binds.append(VarBind(try BERDecoder.oid(name.content), try BERDecoder.value(tag: val.tag, content: val.content)))
        }
        return SNMPMessage(community: String(decoding: community.content, as: UTF8.self), kind: kind,
                           requestID: Int32(truncatingIfNeeded: try BERDecoder.integer(id.content)),
                           errorStatus: Int(try BERDecoder.integer(status.content)),
                           errorIndex: Int(try BERDecoder.integer(index.content)), varbinds: binds)
    }
}
