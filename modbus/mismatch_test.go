package modbus

import (
	"bytes"
	"context"
	"encoding/hex"
	"net"
	"strings"
	"testing"
	"time"
)

func TestTruncatedFrames(t *testing.T) {
	cases := []struct {
		name string
		tr   Transport
		raw  string
		want ErrorKind
	}{
		{"tcp-mbap-truncated", TransportTCP, "0001000000", KindTruncated},
		{"tcp-pdu-truncated", TransportTCP, "00010000000511030200", KindTruncated},
		{"rtu-truncated", TransportRTU, "11030200", KindTruncated},
		{"ascii-truncated", TransportASCII, ":3A31313033303230303742", KindTruncated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			codec, _ := NewCodec(tc.tr)
			var raw []byte
			if strings.HasPrefix(tc.raw, ":") {
				raw = []byte(tc.raw)
			} else {
				raw, _ = hex.DecodeString(tc.raw)
			}
			_, err := codec.DecodeReader(bytes.NewReader(raw))
			me := AsError(err)
			if me == nil || me.Kind != tc.want {
				t.Fatalf("want %s got %v", tc.want, err)
			}
			logVerdict(t, tc.name, raw, err)
		})
	}
}

func TestTransactionMismatch(t *testing.T) {
	mem := NewSlaveMemory(10)
	// 钩子：回一个事务标识被篡改的响应（PDU 正常但帧层 TxnID 错乱）。
	// 这里通过自定义 Dialer 注入代理连接，翻转响应的 TxnID。
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	sl, _ := StartSlave(ln, SlaveConfig{Transport: TransportTCP, Memory: mem, UnitID: 1})
	defer sl.Close()

	dial := func(ctx context.Context) (net.Conn, error) {
		c, err := net.Dial("tcp", sl.Addr().String())
		if err != nil {
			return nil, err
		}
		return &txnFlipConn{Conn: c}, nil
	}
	sess, _ := NewSession(SessionConfig{
		Transport: TransportTCP, UnitID: 1, Dial: dial,
		Timeout: time.Second, MaxRetry: 0,
	})
	defer sess.Close()

	_, err := NewClient(sess).ReadRegisters(context.Background(), RegionHoldingRegisters, 0, 1)
	me := AsError(err)
	if me == nil || me.Kind != KindTransactionMismatch {
		t.Fatalf("want transaction mismatch, got %v", err)
	}
	logVerdict(t, "txn-mismatch", me.Raw, err)
}

// txnFlipConn 把响应帧的事务标识加 1，制造错配。
type txnFlipConn struct {
	net.Conn
	flipped bool
}

func (c *txnFlipConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if !c.flipped && n >= 2 {
		c.flipped = true
		b[1]++ // 篡改事务标识低字节
	}
	return n, err
}

func TestChecksumFailureClassified(t *testing.T) {
	// RTU 坏 CRC 帧直接喂给 codec。
	codec, _ := NewCodec(TransportRTU)
	raw, _ := hex.DecodeString("110302007B0000")
	_, _, err := codec.DecodeBytes(raw)
	me := AsError(err)
	if me == nil || me.Kind != KindChecksum {
		t.Fatalf("want checksum, got %v", err)
	}
	logVerdict(t, "rtu-bad-crc", raw, err)
}
