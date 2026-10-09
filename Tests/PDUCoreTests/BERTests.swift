import XCTest
@testable import PDUCore

final class BERTests: XCTestCase {
    private func hex(_ bytes: [UInt8]) -> String { bytes.map { String(format: "%02x", $0) }.joined(separator: " ") }

    func testGetRequestMatchesKnownBytes() {
        let message = SNMPMessage(community: "public", kind: .get, requestID: 1, varbinds: [VarBind(OID("1.3.6.1.2.1.1.1.0")!)])
        XCTAssertEqual(hex(message.encode()),
                       "30 26 02 01 00 04 06 70 75 62 6c 69 63 a0 19 02 01 01 02 01 00 02 01 00 30 0e 30 0c 06 08 2b 06 01 02 01 01 01 00 05 00")
    }

    func testIntegerEncoding() {
        XCTAssertEqual(hex(BEREncoder.integer(0)), "02 01 00")
        XCTAssertEqual(hex(BEREncoder.integer(127)), "02 01 7f")
        XCTAssertEqual(hex(BEREncoder.integer(128)), "02 02 00 80")
        XCTAssertEqual(hex(BEREncoder.integer(256)), "02 02 01 00")
        XCTAssertEqual(hex(BEREncoder.integer(-1)), "02 01 ff")
        XCTAssertEqual(hex(BEREncoder.integer(-128)), "02 01 80")
        XCTAssertEqual(hex(BEREncoder.integer(-129)), "02 02 ff 7f")
    }

    func testOIDWithLargeSubidentifier() throws {
        let oid = OID("1.3.6.1.4.1.3808.1.1.6")!
        XCTAssertEqual(hex(BEREncoder.oid(oid)), "06 0a 2b 06 01 04 01 9d 60 01 01 06")
        var reader = BERReader(BEREncoder.oid(oid))
        let tlv = try reader.readTLV()
        XCTAssertEqual(try BERDecoder.oid(tlv.content), oid)
    }

    func testRoundTripOfAllValueKinds() throws {
        let binds = [
            VarBind(OID("1.3.6.1.2.1.1.1.0")!, .octetString(Array("PDU81007".utf8))),
            VarBind(OID("1.3.6.1.2.1.1.3.0")!, .unsigned(4_000_000_000)),
            VarBind(OID("1.3.6.1.2.1.1.2.0")!, .oid(OID("1.3.6.1.4.1.3808")!)),
            VarBind(OID("1.3.6.1.2.1.2.1.0")!, .integer(-5)),
            VarBind(OID("1.3.6.1.2.1.2.2.0")!, .null)
        ]
        let message = SNMPMessage(community: "x", kind: .response, requestID: 123456, errorStatus: 2, errorIndex: 3, varbinds: binds)
        let decoded = try SNMPMessage.decode(message.encode())
        XCTAssertEqual(decoded.requestID, 123456)
        XCTAssertEqual(decoded.errorStatus, 2)
        XCTAssertEqual(decoded.errorIndex, 3)
        XCTAssertEqual(decoded.varbinds[0].value.stringValue, "PDU81007")
        XCTAssertEqual(decoded.varbinds[1].value.intValue, 4_000_000_000)
        XCTAssertEqual(decoded.varbinds[2].value, .oid(OID("1.3.6.1.4.1.3808")!))
        XCTAssertEqual(decoded.varbinds[3].value.intValue, -5)
        XCTAssertEqual(decoded.varbinds[4].value, .null)
    }

    func testLongLengthsRoundTrip() throws {
        let text = String(repeating: "a", count: 300)
        let message = SNMPMessage(community: "public", kind: .response, requestID: 1, varbinds: [VarBind(OID("1.3.6")!, .octetString(Array(text.utf8)))])
        XCTAssertEqual(try SNMPMessage.decode(message.encode()).varbinds[0].value.stringValue, text)
    }

    func testRealGaugeAnswerFromAnAgent() throws {
        // Response, Gauge32 value 394 (a 2 byte number) for 1.3.6.1.4.1.3808.1.1.6.6.2.4.1.7.11
        let answer = SNMPMessage(community: "public", kind: .response, requestID: 9,
                                 varbinds: [VarBind(OID("1.3.6.1.4.1.3808.1.1.6.6.2.4.1.7.11")!, .unsigned(394))])
        XCTAssertEqual(try SNMPMessage.decode(answer.encode()).varbinds[0].value.intValue, 394)
    }

    func testGarbageIsRejectedNotCrashing() {
        XCTAssertThrowsError(try SNMPMessage.decode([]))
        XCTAssertThrowsError(try SNMPMessage.decode([0x30, 0x05, 0x02]))
        XCTAssertThrowsError(try SNMPMessage.decode([0x04, 0x01, 0x41]))
        XCTAssertThrowsError(try SNMPMessage.decode([0x30, 0x84, 0xff, 0xff, 0xff, 0xff]))
    }

    func testOIDParsing() {
        XCTAssertEqual(OID(".1.3.6")!.components, [1, 3, 6])
        XCTAssertNil(OID("1.x"))
        XCTAssertNil(OID(""))
        XCTAssertTrue(OID("1.3.6.1.4")!.hasPrefix(OID("1.3.6")!))
        XCTAssertTrue(OID("1.3.6.1")! < OID("1.3.6.1.0")!)
    }
}
