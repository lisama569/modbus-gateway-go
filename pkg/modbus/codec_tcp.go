package modbus

import (
	"encoding/binary"
	"io"
	"strconv"
)

// tcpCodec 实现 Modbus TCP(MBAP + PDU)分帧。
//
// 帧布局:
//
//	Transaction ID (2) | Protocol ID (2,=0) | Length (2) | Unit ID (1) | PDU (n)
//	Length 计数字段覆盖 Unit ID 与 PDU。
type tcpCodec struct{}

func (c *tcpCodec) Name() string { return string(TransportTCP) }

func (c *tcpCodec) Encode(f *Frame) ([]byte, error) {
	if len(f.PDU) > 253 {
		return nil, newError(KindLengthMismatch,
			"tcp PDU too long: %d bytes (max 253)", len(f.PDU))
	}
	raw := make([]byte, 7+len(f.PDU))
	binary.BigEndian.PutUint16(raw[0:], f.TransactionID)
	binary.BigEndian.PutUint16(raw[2:], 0) // Modbus 协议标识
	binary.BigEndian.PutUint16(raw[4:], uint16(len(f.PDU)+1))
	raw[6] = f.Unit
	copy(raw[7:], f.PDU)
	f.Raw = raw
	return raw, nil
}

func (c *tcpCodec) Decode(r io.Reader, dir Direction) (*Frame, error) {
	header := make([]byte, 7)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, mapReadErr(err, nil)
	}
	txn := binary.BigEndian.Uint16(header[0:])
	pid := binary.BigEndian.Uint16(header[2:])
	length := binary.BigEndian.Uint16(header[4:])
	unit := header[6]

	if pid != 0 {
		return nil, &Error{
			Kind: KindLengthMismatch,
			Msg:  "tcp protocol identifier mismatch: got " + strconv.Itoa(int(pid)) + ", want 0",
			Raw:  append([]byte(nil), header...),
		}
	}
	if length < 1 {
		return nil, &Error{
			Kind: KindLengthMismatch,
			Msg:  "tcp length field must count at least unit id, got 0",
			Raw:  append([]byte(nil), header...),
		}
	}
	pduLen := int(length) - 1
	if pduLen > 253 {
		return nil, &Error{
			Kind: KindLengthMismatch,
			Msg:  "tcp length field exceeds maximum PDU size: " + strconv.Itoa(pduLen),
			Raw:  append([]byte(nil), header...),
		}
	}

	pdu := make([]byte, pduLen)
	n, err := io.ReadFull(r, pdu)
	if err != nil {
		raw := append(append([]byte(nil), header...), pdu[:n]...)
		return nil, mapBodyReadErr(err, raw, pduLen, n)
	}

	raw := append(append([]byte(nil), header...), pdu...)
	return &Frame{
		Unit:          unit,
		PDU:           pdu,
		Raw:           raw,
		TransactionID: txn,
	}, nil
}
