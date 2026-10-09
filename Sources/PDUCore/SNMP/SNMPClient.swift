import Foundation
#if canImport(Darwin)
import Darwin
#elseif canImport(Glibc)
import Glibc
#endif

public enum SNMPError: Error, LocalizedError, Equatable {
    case timeout
    case cannotResolve(String)
    case socket(String)
    case malformed(String)
    /// SNMPv1 error-status 1: the answer would not fit in a packet the device is willing to send (or the request was too big).
    case tooBig
    /// SNMPv1 error-status 2: one of the requested objects does not exist. `index` is 1-based.
    case noSuchName(index: Int)
    /// Any other error-status of the answer (3 badValue, 4 readOnly, 5 genErr, ...).
    case agent(status: Int, index: Int)
    public var errorDescription: String? {
        switch self {
        case .timeout: return "No answer (timeout)"
        case .cannotResolve(let host): return "Cannot resolve \(host)"
        case .socket(let message): return "Network error: \(message)"
        case .malformed(let what): return "Unexpected answer (\(what))"
        case .tooBig: return "The device cannot answer such a big request (tooBig)"
        case .noSuchName: return "The device does not have this object"
        case .agent(let status, _):
            switch status {
            case 3: return "The device refused the value (badValue)"
            case 4: return "The device refused the change: the community is read-only (readOnly)"
            case 5: return "The device reported a general error (genErr)"
            default: return "The device answered with SNMP error \(status)"
            }
        }
    }
}

/// What a driver needs from the network. `UDPSNMPClient` is the real one; tests use fakes.
public protocol SNMPTransport: Sendable {
    /// One GET with all the OIDs. Throws `.noSuchName(index:)` when the agent says one of them does not exist.
    func get(_ oids: [OID]) async throws -> [VarBind]
    func getNext(_ oid: OID) async throws -> VarBind
    func set(_ binds: [VarBind]) async throws
}

extension SNMPTransport {
    /// GET that survives objects that do not exist: the offending OID is dropped and the rest is asked again.
    /// Returns only the OIDs the device has. Requests are split into groups so that one UDP packet stays small.
    public func getAvailable(_ oids: [OID], chunk: Int = 8) async throws -> [OID: SNMPValue] {
        var result: [OID: SNMPValue] = [:]
        var queue: [[OID]] = stride(from: 0, to: oids.count, by: max(1, chunk)).map { Array(oids[$0..<min($0 + max(1, chunk), oids.count)]) }
        queue.reverse()
        while var pending = queue.popLast() {
            while !pending.isEmpty {
                do {
                    for bind in try await get(pending) where !bind.value.isMissing { result[bind.oid] = bind.value }
                    pending = []
                } catch SNMPError.tooBig {
                    // Many PDUs only answer small packets: the same objects are asked again in two halves, down to one by one.
                    guard pending.count > 1 else { throw SNMPError.tooBig }
                    let half = pending.count / 2
                    queue.append(Array(pending[half...]))
                    pending = Array(pending[..<half])
                } catch SNMPError.noSuchName(let position) {
                    // Some agents leave the index at 0: then every object is asked alone.
                    if position >= 1 && position <= pending.count {
                        pending.remove(at: position - 1)
                    } else {
                        for oid in pending {
                            do { for b in try await get([oid]) where !b.value.isMissing { result[b.oid] = b.value } }
                            catch SNMPError.noSuchName { continue }
                        }
                        pending = []
                    }
                }
            }
        }
        return result
    }
}

/// Where the debug log goes (set by the application). Every request and answer is written as one line.
public enum SNMPDebug {
    public nonisolated(unsafe) static var log: (@Sendable (String) -> Void)?
}

private final class RequestCounter: @unchecked Sendable {
    private let lock = NSLock()
    private var value: Int32 = Int32.random(in: 1...1_000_000)
    func next() -> Int32 {
        lock.lock(); defer { lock.unlock() }
        value = value == Int32.max ? 1 : value + 1
        return value
    }
}

/// SNMPv1 over UDP with plain POSIX sockets (one socket per request: the agents are polled every few seconds).
public struct UDPSNMPClient: SNMPTransport {
    public var host: String
    public var port: UInt16
    public var readCommunity: String
    public var writeCommunity: String
    public var timeout: TimeInterval
    public var retries: Int
    private static let counter = RequestCounter()

    public init(host: String, port: UInt16 = 161, readCommunity: String, writeCommunity: String? = nil,
                timeout: TimeInterval = 2.0, retries: Int = 1) {
        self.host = host; self.port = port; self.readCommunity = readCommunity
        self.writeCommunity = (writeCommunity?.isEmpty == false) ? writeCommunity! : readCommunity
        self.timeout = timeout; self.retries = retries
    }

    public func get(_ oids: [OID]) async throws -> [VarBind] {
        try await run(SNMPMessage(community: readCommunity, kind: .get, requestID: Self.counter.next(), varbinds: oids.map { VarBind($0) }))
    }
    public func getNext(_ oid: OID) async throws -> VarBind {
        let binds = try await run(SNMPMessage(community: readCommunity, kind: .getNext, requestID: Self.counter.next(), varbinds: [VarBind(oid)]))
        guard let first = binds.first else { throw SNMPError.malformed("empty answer") }
        return first
    }
    public func set(_ binds: [VarBind]) async throws {
        _ = try await run(SNMPMessage(community: writeCommunity, kind: .set, requestID: Self.counter.next(), varbinds: binds))
    }

    private func run(_ message: SNMPMessage) async throws -> [VarBind] {
        let client = self
        return try await withCheckedThrowingContinuation { continuation in
            DispatchQueue.global(qos: .userInitiated).async {
                do { continuation.resume(returning: try client.exchange(message)) } catch { continuation.resume(throwing: error) }
            }
        }
    }

    /// Blocking request and answer. Runs on a background queue.
    func exchange(_ message: SNMPMessage) throws -> [VarBind] {
        let packet = message.encode()
        let fd = try openSocket()
        defer { close(fd) }
        var lastError: Error = SNMPError.timeout
        for _ in 0...max(0, retries) {
            do {
                try send(packet, on: fd)
                let deadline = Date().addingTimeInterval(timeout)
                while true {
                    let remaining = deadline.timeIntervalSinceNow
                    if remaining <= 0 { throw SNMPError.timeout }
                    guard let data = try receive(on: fd, timeoutMs: Int32(remaining * 1000) + 1) else { throw SNMPError.timeout }
                    guard let answer = try? SNMPMessage.decode(data), answer.kind == .response, answer.requestID == message.requestID else { continue }
                    SNMPDebug.log?("\(host):\(port) \(message.kind) \(message.varbinds.count) object(s) -> status \(answer.errorStatus) index \(answer.errorIndex), \(answer.varbinds.count) answer(s)")
                    switch answer.errorStatus {
                    case 0: return answer.varbinds
                    case 1: throw SNMPError.tooBig
                    case 2: throw SNMPError.noSuchName(index: answer.errorIndex)
                    default: throw SNMPError.agent(status: answer.errorStatus, index: answer.errorIndex)
                    }
                }
            } catch SNMPError.timeout {
                SNMPDebug.log?("\(host):\(port) \(message.kind) \(message.varbinds.count) object(s) -> no answer within \(timeout) s")
                lastError = SNMPError.timeout
            }
        }
        throw lastError
    }

    private func openSocket() throws -> Int32 {
        var hints = addrinfo()
        hints.ai_family = AF_UNSPEC
        #if os(Linux)
        hints.ai_socktype = Int32(SOCK_DGRAM.rawValue)
        #else
        hints.ai_socktype = SOCK_DGRAM
        #endif
        var info: UnsafeMutablePointer<addrinfo>?
        let status = getaddrinfo(host, String(port), &hints, &info)
        guard status == 0, let first = info else { throw SNMPError.cannotResolve(host) }
        defer { freeaddrinfo(info) }
        let fd = socket(first.pointee.ai_family, first.pointee.ai_socktype, first.pointee.ai_protocol)
        guard fd >= 0 else { throw SNMPError.socket(String(cString: strerror(errno))) }
        guard connect(fd, first.pointee.ai_addr, first.pointee.ai_addrlen) == 0 else {
            let message = String(cString: strerror(errno)); close(fd); throw SNMPError.socket(message)
        }
        return fd
    }

    private func send(_ packet: [UInt8], on fd: Int32) throws {
        let sent = packet.withUnsafeBytes { Glibc_or_Darwin_send(fd, $0.baseAddress, packet.count) }
        guard sent == packet.count else { throw SNMPError.socket(String(cString: strerror(errno))) }
    }

    /// nil on timeout.
    private func receive(on fd: Int32, timeoutMs: Int32) throws -> [UInt8]? {
        var descriptor = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
        let ready = poll(&descriptor, 1, timeoutMs)
        if ready == 0 { return nil }
        if ready < 0 { if errno == EINTR { return nil }; throw SNMPError.socket(String(cString: strerror(errno))) }
        var buffer = [UInt8](repeating: 0, count: 65535)
        let count = buffer.withUnsafeMutableBytes { recv(fd, $0.baseAddress, $0.count, 0) }
        if count < 0 {
            // A connected UDP socket reports "connection refused" when the host answered with ICMP port unreachable.
            throw SNMPError.socket(String(cString: strerror(errno)))
        }
        return Array(buffer.prefix(count))
    }
}

@inline(__always) private func Glibc_or_Darwin_send(_ fd: Int32, _ pointer: UnsafeRawPointer?, _ count: Int) -> Int {
    #if canImport(Darwin)
    return Darwin.send(fd, pointer, count, 0)
    #else
    return Glibc.send(fd, pointer, count, 0)
    #endif
}
