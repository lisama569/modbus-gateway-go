package modbus

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// logVerdict 打印原始报文与归一化判定结果（测试日志硬性要求）。
func logVerdict(t *testing.T, name string, raw []byte, err error) {
	t.Helper()
	kind := "ok"
	except := ""
	if me := AsError(err); me != nil {
		kind = me.Kind.String()
		if me.Kind == KindModbusException {
			except = " exception=" + me.Except.String()
		}
	} else if err != nil {
		kind = "non-modbus-error"
	}
	t.Logf("[%s] raw=%s -> verdict=%s%s err=%v",
		name, strings.ToUpper(hex.EncodeToString(raw)), kind, except, err)
}

func TestCRC16KnownVector(t *testing.T) {
	// 读保持寄存器：从站 01，FC03，地址 0000，数量 000A -> CRC C5CD（低字节在前）
	frame, _ := hex.DecodeString("01030000000A")
	got := CRC16(frame)
	if got != 0xCDC5 {
		t.Fatalf("CRC=%04X want CDC5", got)
	}
	full := AppendCRC(frame)
	if !CheckCRC(full) || CheckCRC(append(frame, 0x00, 0x00)) {
		t.Fatal("CheckCRC behavior wrong")
	}
}

func TestLRC(t *testing.T) {
	// 标准样例：01 03 00 00 00 0A -> LRC F2
	data, _ := hex.DecodeString("01030000000A")
	if LRC(data) != 0xF2 {
		t.Fatalf("LRC=%02X want F2", LRC(data))
	}
}

func TestCodecRoundTrip(t *testing.T) {
	adu := ADU{UnitID: 0x11, TxnID: 0x1234, PDU: []byte{0x03, 0x02, 0x00, 0x7B}}
	for _, tr := range []Transport{TransportTCP, TransportRTU, TransportASCII} {
		codec, err := NewCodec(tr)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := codec.Encode(adu)
		if err != nil {
			t.Fatal(err)
		}
		got, rest, derr := codec.DecodeBytes(wire)
		if derr != nil {
			t.Fatalf("%s decode: %v", tr, derr)
		}
		if len(rest) != 0 || got.UnitID != adu.UnitID || !bytes.Equal(got.PDU, adu.PDU) {
			t.Fatalf("%s roundtrip mismatch: %+v rest=%x", tr, got, rest)
		}
		if tr == TransportTCP && got.TxnID != adu.TxnID {
			t.Fatalf("%s txn mismatch", tr)
		}
		logVerdict(t, string(tr)+"-roundtrip", wire, nil)
	}
}

func TestCodecMalformed(t *testing.T) {
	cases := []struct {
		name string
		tr   Transport
		raw  string
		want ErrorKind
	}{
		{"tcp-truncated", TransportTCP, "0001000000", KindTruncated},
		{"tcp-bad-proto", TransportTCP, "000000010003110302007B", KindTransactionMismatch},
		{"rtu-bad-crc", TransportRTU, "110302007B0000", KindChecksum},
		{"rtu-unknown-fc", TransportRTU, "11450000", KindUnsupportedFunction},
		{"ascii-bad-lrc", TransportASCII, ":3A0302007BFF\r\n", KindChecksum},
		{"ascii-odd-hex", TransportASCII, ":3A302007BFB\r\n", KindLengthMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			codec, _ := NewCodec(tc.tr)
			raw, _ := hex.DecodeString(tc.raw)
			// ASCII 用例直接给可打印帧
			if strings.HasPrefix(tc.raw, ":") {
				raw = []byte(tc.raw)
			}
			var err error
			if tc.tr == TransportTCP && len(raw) < 7 {
				// 不足 MBAP 头走读流路径验证截断
				_, rerr := codec.DecodeReader(bytes.NewReader(raw))
				err = rerr
			} else {
				_, _, err = codec.DecodeBytes(raw)
				if err == ErrNeedMore {
					_, err = codec.DecodeReader(bytes.NewReader(raw))
				}
			}
			me := AsError(err)
			if me == nil {
				t.Fatalf("want error %s, got nil (raw=%x)", tc.want, raw)
			}
			if me.Kind != tc.want {
				t.Fatalf("want %s got %s", tc.want, me.Kind)
			}
			logVerdict(t, tc.name, raw, err)
		})
	}
}

func TestPDUExceptionClassification(t *testing.T) {
	p := NewPDU()
	// 0x83 0x02 = FC3 异常：非法数据地址
	_, err := p.DecodeResponse([]byte{0x83, 0x02}, FCReadHoldingRegisters)
	me := AsError(err)
	if me == nil || me.Kind != KindModbusException || me.Except != ExcIllegalDataAddress {
		t.Fatalf("bad classification: %v", err)
	}
	logVerdict(t, "exception", []byte{0x83, 0x02}, err)

	// 正常数据不得与异常混淆
	resp, err := p.DecodeResponse([]byte{0x03, 0x02, 0x00, 0x7B}, FCReadHoldingRegisters)
	if err != nil || len(resp.Data) != 2 {
		t.Fatalf("normal response misclassified: %v", err)
	}
}
