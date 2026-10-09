import Foundation
#if canImport(Darwin)
import Darwin
#else
import Glibc
#endif
@testable import PDUCore

/// An SNMPv1 agent on 127.0.0.1 with a table of values, so that the real UDP client and drivers are tested end to end.
final class FakeAgent: @unchecked Sendable {
    private let lock = NSLock()
    private var values: [OID: SNMPValue] = [:]
    private var stopped = false
    private var fd: Int32 = -1
    private(set) var port: UInt16 = 0
    let readCommunity: String, writeCommunity: String
    /// Called after a SET so that the test agent can mirror the change into a status object.
    var onSet: (@Sendable (OID, SNMPValue, FakeAgent) -> Void)?
    private(set) var requests: [SNMPMessage] = []
    /// A request with more objects than this is answered with error-status 1 (tooBig), like PDUs with a small packet limit do.
    var maxVarbinds = Int.max

    init(read: String = "public", write: String = "private") throws {
        readCommunity = read; writeCommunity = write
        #if os(Linux)
        let datagram = Int32(SOCK_DGRAM.rawValue)
#else
        let datagram = SOCK_DGRAM
#endif
        fd = socket(AF_INET, datagram, 0)
        var address = sockaddr_in()
        address.sin_family = sa_family_t(AF_INET)
        address.sin_addr.s_addr = inet_addr("127.0.0.1")
        address.sin_port = 0
        let bound = withUnsafePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) }
        }
        guard bound == 0 else { throw SNMPError.socket("bind") }
        var length = socklen_t(MemoryLayout<sockaddr_in>.size)
        _ = withUnsafeMutablePointer(to: &address) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(fd, $0, &length) } }
        port = UInt16(bigEndian: address.sin_port)
        Thread.detachNewThread { [self] in loop() }
    }

    func stop() { lock.lock(); stopped = true; lock.unlock() }

    subscript(_ oid: String) -> SNMPValue? {
        get { lock.withLock { values[OID(oid)!] } }
        set { lock.withLock { values[OID(oid)!] = newValue } }
    }
    func set(_ oid: OID, _ value: SNMPValue) { lock.withLock { values[oid] = value } }
    func value(_ oid: OID) -> SNMPValue? { lock.withLock { values[oid] } }
    var receivedRequests: [SNMPMessage] { lock.withLock { requests } }

    var transport: UDPSNMPClient { UDPSNMPClient(host: "127.0.0.1", port: port, readCommunity: readCommunity, writeCommunity: writeCommunity, timeout: 1, retries: 0) }

    private func loop() {
        var buffer = [UInt8](repeating: 0, count: 65535)
        while true {
            if lock.withLock({ stopped }) { close(fd); return }
            var descriptor = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
            if poll(&descriptor, 1, 50) <= 0 { continue }
            var from = sockaddr_storage()
            var fromLength = socklen_t(MemoryLayout<sockaddr_storage>.size)
            let count = buffer.withUnsafeMutableBytes { raw in
                withUnsafeMutablePointer(to: &from) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { recvfrom(fd, raw.baseAddress, raw.count, 0, $0, &fromLength) } }
            }
            guard count > 0, let request = try? SNMPMessage.decode(Array(buffer.prefix(count))) else { continue }
            lock.withLock { requests.append(request) }
            guard let answer = respond(to: request) else { continue }
            let bytes = answer.encode()
            _ = withUnsafePointer(to: &from) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { sendto(fd, bytes, bytes.count, 0, $0, fromLength) } }
        }
    }

    private func respond(to request: SNMPMessage) -> SNMPMessage? {
        let expected = request.kind == .set ? writeCommunity : readCommunity
        guard request.community == expected || (request.kind == .get && request.community == writeCommunity) else { return nil }   // silently dropped, like a real agent
        var answer = SNMPMessage(community: request.community, kind: .response, requestID: request.requestID, varbinds: request.varbinds)
        if request.varbinds.count > maxVarbinds { answer.errorStatus = 1; answer.errorIndex = 0; return answer }
        switch request.kind {
        case .get:
            for (i, bind) in request.varbinds.enumerated() {
                guard let v = value(bind.oid) else { answer.errorStatus = 2; answer.errorIndex = i + 1; answer.varbinds = request.varbinds; return answer }
                answer.varbinds[i].value = v
            }
        case .getNext:
            let keys = lock.withLock { values.keys.sorted() }
            if let next = keys.first(where: { $0 > request.varbinds[0].oid }), let v = value(next) { answer.varbinds = [VarBind(next, v)] }
            else { answer.errorStatus = 2; answer.errorIndex = 1 }
        case .set:
            for bind in request.varbinds {
                guard value(bind.oid) != nil else { answer.errorStatus = 2; answer.errorIndex = 1; return answer }
                set(bind.oid, bind.value)
                onSet?(bind.oid, bind.value, self)
            }
        case .response: return nil
        }
        return answer
    }
}
