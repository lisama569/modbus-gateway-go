package modbus

import (
	"bytes"
	"testing"
)

// logFrame 在单测日志中打印原始报文与归一化判定结果。
func logFrame(t *testing.T, stage, transport string, raw []byte, result string) {
	t.Helper()
	t.Logf("[%s/%s] raw=%x => normalized: %s", transport, stage, raw, result)
}

func TestCodecRoundTripAllTransports(t *testing.T) {
	reqs := []*Request{
		{Unit: 1, Function: FCReadHoldingRegisters, Address: 0x0102, Quantity: 3},
		{Unit: 2, Function: FCReadCoils, Address: 0x0000, Quantity: 9},
		{Unit: 3, Function: FCWriteSingleRegister, Address: 0x0010, Value: 0x1234},
		{Unit: 4, Function: FCWriteMultipleRegisters, Address: 0x0020,
			Quantity: 2, Bytes: []byte{0xAA, 0xBB, 0xCC, 0xDD}},
	}
	resps := []*Response{
		{Function: FCReadHoldingRegisters, Registers: []uint16{0x1111, 0x2222, 0x3333}},
		{Function: FCReadCoils, Bits: []bool{true, false, true, true, false, false, false, false, true}},
		{Function: FCWriteSingleRegister, Address: 0x0010, Value: 0x1234},
		{Function: FCWriteMultipleRegisters, Address: 0x0020, Quantity: 2},
	}

	for _, tr := range []Transport{TransportTCP, TransportRTU, TransportASCII} {
		codec, err := NewCodec(tr)
		if err != nil {
			t.Fatalf("NewCodec(%s): %v", tr, err)
		}
		for i, req := range reqs {
			pdu, _ := req.EncodePDU()
			// 请求帧往返
			rf := &Frame{Unit: req.Unit, PDU: pdu, TransactionID: uint16(i + 1)}
			raw, err := codec.Encode(rf)
			if err != nil {
				t.Fatalf("%s encode req#%d: %v", tr, i, err)
			}
			got, err := codec.Decode(bytes.NewReader(raw), DirRequest)
			if err != nil {
				t.Fatalf("%s decode req#%d raw=%x: %v", tr, i, raw, err)
			}
			logFrame(t, "request", string(tr), raw, "OK")
			if got.Unit != req.Unit || !bytes.Equal(got.PDU, pdu) {
				t.Fatalf("%s req#%d frame mismatch", tr, i)
			}

			// 响应帧往返
			rpdu, _ := resps[i].EncodePDU()
			respFrame := &Frame{Unit: req.Unit, PDU: rpdu, TransactionID: uint16(i + 1)}
			rraw, err := codec.Encode(respFrame)
			if err != nil {
				t.Fatalf("%s encode resp#%d: %v", tr, i, err)
			}
			dresp, err := codec.Decode(bytes.NewReader(rraw), DirResponse)
			if err != nil {
				t.Fatalf("%s decode resp#%d: %v", tr, i, err)
			}
			decoded, err := DecodeResponsePDU(req.Function, dresp.PDU)
			if err != nil {
				t.Fatalf("%s decode response PDU#%d: %v", tr, i, err)
			}
			logFrame(t, "response", string(tr), rraw, "OK")
			if req.Function == FCReadHoldingRegisters &&
				!equalU16(decoded.Registers, resps[i].Registers) {
				t.Fatalf("%s registers mismatch: %v", tr, decoded.Registers)
			}
		}
	}
}

func TestMalformedAndTruncatedFrames(t *testing.T) {
	cases := []struct {
		name      string
		transport Transport
		raw       []byte
		want      ErrorKind
	}{
		{"tcp truncated header", TransportTCP, []byte{0, 1, 0, 0}, KindTruncated},
		{"tcp length exceeds body", TransportTCP,
			[]byte{0, 1, 0, 0, 0, 6, 1, 3, 0x02}, KindLengthMismatch},
		{"tcp bad protocol id", TransportTCP,
			[]byte{0, 1, 0, 1, 0, 2, 1, 3}, KindLengthMismatch},
		{"rtu truncated", TransportRTU, []byte{1, 3, 0, 10}, KindTruncated},
		{"rtu crc failure", TransportRTU,
			encodeRTUOrDie(t, 1, []byte{3, 0, 0, 0, 2}, true), KindChecksum},
		{"rtu unsupported fc", TransportRTU, []byte{1, 0x09, 0x00, 0x00, 0x00, 0x00, 0x00},
			KindUnsupportedFunction},
		{"ascii truncated (no CRLF)", TransportASCII, []byte(":"), KindClosed},
		{"ascii bad hex", TransportASCII, []byte(":ZZZZ\r\n"), KindChecksum},
		{"ascii too short", TransportASCII, []byte(":01\r\n"), KindTruncated},
		{"ascii lrc failure", TransportASCII,
			[]byte(":" + "010300000002" + "00\r\n"), KindChecksum},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			codec, _ := NewCodec(tc.transport)
			_, err := codec.Decode(bytes.NewReader(tc.raw), DirResponse)
			got := KindOf(err)
			logFrame(t, "decode", string(tc.transport), tc.raw,
				"error kind="+kindText(got)+" ("+errString(err)+")")
			if got != tc.want {
				t.Fatalf("want kind %d(%s), got %d(%s) err=%v",
					tc.want, kindText(tc.want), got, kindText(got), err)
			}
		})
	}
}

func TestExceptionResponseClassification(t *testing.T) {
	for _, tr := range []Transport{TransportTCP, TransportRTU, TransportASCII} {
		codec, _ := NewCodec(tr)
		pdu := ExceptionResponsePDU(FCReadHoldingRegisters, ExcIllegalDataAddress)
		f := &Frame{Unit: 1, PDU: pdu}
		raw, _ := codec.Encode(f)
		df, err := codec.Decode(bytes.NewReader(raw), DirResponse)
		if err != nil {
			t.Fatalf("%s: %v", tr, err)
		}
		_, err = DecodeResponsePDU(FCReadHoldingRegisters, df.PDU)
		if KindOf(err) != KindExceptionResponse {
			t.Fatalf("%s: want exception, got %v", tr, err)
		}
		me := err.(*Error)
		logFrame(t, "exception", string(tr), raw,
			"exception code=0x"+hex2(byte(me.ExceptionCode))+" ("+me.ExceptionCode.String()+")")
		if ExceptionCode(me.ExceptionCode) != ExcIllegalDataAddress {
			t.Fatalf("%s: wrong exception code %d", tr, me.ExceptionCode)
		}
	}
}

func TestTCPTransactionAndUnitMismatch(t *testing.T) {
	codec, _ := NewCodec(TransportTCP)
	// 正确帧 txn=7, unit=1
	pdu, _ := (&Response{Function: FCReadHoldingRegisters, Registers: []uint16{1, 2}}).EncodePDU()
	raw, _ := codec.Encode(&Frame{Unit: 1, PDU: pdu, TransactionID: 7})

	// 篡改事务标识
	badTxn := append([]byte(nil), raw...)
	badTxn[0], badTxn[1] = 0xFF, 0xFF
	f, err := codec.Decode(bytes.NewReader(badTxn), DirResponse)
	if err != nil {
		t.Fatal(err)
	}
	if f.TransactionID == 7 {
		t.Fatal("expected tampered transaction id")
	}
	logFrame(t, "txn", "tcp", badTxn, "frame decoded; mismatch checked at session layer")
}

func encodeRTUOrDie(t *testing.T, unit byte, pdu []byte, corrupt bool) []byte {
	t.Helper()
	codec := &rtuCodec{}
	f := &Frame{Unit: unit, PDU: pdu}
	raw, err := codec.Encode(f)
	if err != nil {
		t.Fatal(err)
	}
	if corrupt {
		raw[len(raw)-1] ^= 0xFF
	}
	return raw
}

func equalU16(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
