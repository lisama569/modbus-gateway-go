package modbus

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// MBAP 头：事务标识(2) 协议标识(2) 长度(2) 单元标识(1)。
const (
	mbapHeaderSize = 7
	mbapProtoID    = 0
)

// tcpCodec 实现 Modbus TCP 帧（MBAP 头 + PDU）。
type tcpCodec struct {
	pdu    PDU
	nextTx uint16 // 编码侧自增事务标识（会话层也可显式指定 ADU.TxnID）
}

func newTCPCodec() Codec { return &tcpCodec{pdu: NewPDU()} }

func (c *tcpCodec) Transport() Transport         { return TransportTCP }
func (c *tcpCodec) InterFrameGap() time.Duration { return 0 }

// Encode 编码 MBAP+PDU。ADU.TxnID 为 0 时由 codec 分配（首次从 1 开始）。
func (c *tcpCodec) Encode(adu ADU) ([]byte, error) {
	if len(adu.PDU) == 0 {
		return nil, NewError(KindInvalidArgument, "empty PDU")
	}
	txn := adu.TxnID
	if txn == 0 {
		if c.nextTx == 0 || c.nextTx == 0xFFFF {
			c.nextTx = 1
		}
		txn = c.nextTx
		c.nextTx++
	}
	length := uint16(1 + len(adu.PDU)) // 单元标识 + PDU
	wire := make([]byte, mbapHeaderSize+len(adu.PDU))
	binary.BigEndian.PutUint16(wire[0:2], txn)
	binary.BigEndian.PutUint16(wire[2:4], mbapProtoID)
	binary.BigEndian.PutUint16(wire[4:6], length)
	wire[6] = adu.UnitID
	copy(wire[7:], adu.PDU)
	return wire, nil
}

// DecodeReader 读取并解码一个完整 TCP 帧。
func (c *tcpCodec) DecodeReader(r io.Reader) (ADU, error) {
	head := make([]byte, mbapHeaderSize)
	if _, err := io.ReadFull(r, head); err != nil {
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			return ADU{}, NewError(KindTruncated, "MBAP header truncated").WithRaw(head)
		}
		return ADU{}, WrapError(KindConnClosed, "read MBAP failed", err)
	}
	proto := binary.BigEndian.Uint16(head[2:4])
	if proto != mbapProtoID {
		return ADU{}, NewError(KindTransactionMismatch,
			fmt.Sprintf("protocol id must be 0, got %d", proto)).WithRaw(head)
	}
	length := binary.BigEndian.Uint16(head[4:6])
	if length < 1 {
		return ADU{}, NewError(KindLengthMismatch,
			"MBAP length must include at least unit id").WithRaw(head)
	}
	body := make([]byte, int(length)-1) // 长度含单元标识，减去后为 PDU
	if len(body) > 0 {
		if _, err := io.ReadFull(r, body); err != nil {
			return ADU{}, NewError(KindTruncated, "PDU truncated per MBAP length").
				WithRaw(append(append([]byte{}, head...), body...))
		}
	}
	return ADU{TxnID: binary.BigEndian.Uint16(head[0:2]), UnitID: head[6], PDU: body}, nil
}

// DecodeBytes 从缓冲中解析一个 TCP 帧。
func (c *tcpCodec) DecodeBytes(buf []byte) (ADU, []byte, error) {
	if len(buf) < mbapHeaderSize {
		return ADU{}, buf, ErrNeedMore
	}
	length := int(binary.BigEndian.Uint16(buf[4:6]))
	total := mbapHeaderSize + (length - 1)
	if length < 1 {
		return ADU{}, buf, NewError(KindLengthMismatch, "illegal MBAP length 0").WithRaw(buf[:mbapHeaderSize])
	}
	if len(buf) < total {
		return ADU{}, buf, ErrNeedMore
	}
	frame := buf[:total]
	if proto := binary.BigEndian.Uint16(frame[2:4]); proto != mbapProtoID {
		return ADU{}, buf[total:], NewError(KindTransactionMismatch,
			fmt.Sprintf("protocol id must be 0, got %d", proto)).WithRaw(frame)
	}
	adu := ADU{
		TxnID:  binary.BigEndian.Uint16(frame[0:2]),
		UnitID: frame[6],
		PDU:    append([]byte(nil), frame[7:]...),
	}
	return adu, buf[total:], nil
}

// DialTCP 是便捷的 TCP 拨号封装。
func DialTCP(addr string) (net.Conn, error) { return net.Dial("tcp", addr) }
