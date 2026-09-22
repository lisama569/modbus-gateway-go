package modbus

import (
	"io"
)

// rtuCodec 实现 Modbus RTU(地址 + PDU + CRC16)分帧。
//
// 帧布局:
//
//	Slave Address (1) | Function (1) | Data (n) | CRC16 (2, 低字节在前)
//
// RTU 在线上没有长度字段,本实现依据功能码与数据域字节计数推断帧长
// (Modbus RTU 在串口上依靠 3.5 字符静默间隔分帧;本地承载在流上,
// 改由确定性的长度推断分帧,语义等价且可复现)。
type rtuCodec struct{}

func (c *rtuCodec) Name() string { return string(TransportRTU) }

func (c *rtuCodec) Encode(f *Frame) ([]byte, error) {
	if len(f.PDU) > 252 {
		return nil, newError(KindLengthMismatch,
			"rtu PDU too long: %d bytes (max 252)", len(f.PDU))
	}
	raw := make([]byte, 0, len(f.PDU)+3)
	raw = append(raw, f.Unit)
	raw = append(raw, f.PDU...)
	crc := crc16(raw)
	raw = append(raw, byte(crc), byte(crc>>8)) // CRC 低字节在前
	f.Raw = raw
	return raw, nil
}

func (c *rtuCodec) Decode(r io.Reader, dir Direction) (*Frame, error) {
	pr := newPeekReader(r)

	addr, err := pr.readByte()
	if err != nil {
		return nil, mapReadErr(err, nil)
	}
	fcByte, err := pr.readByte()
	if err != nil {
		return nil, mapReadErr(err, []byte{addr})
	}
	fc := FunctionCode(fcByte)

	// 异常响应:功能码 + 异常码 + CRC,固定 5 字节。
	if fcByte&0x80 != 0 {
		rest, raw, err := pr.readRest(3, []byte{addr, fcByte}) // 异常码 + CRC2
		if err != nil {
			return nil, mapReadErr(err, raw)
		}
		return c.checkAndFrame(addr, fc, append([]byte{fcByte}, rest[:1]...), raw)
	}

	if !fc.supported() {
		// 未知功能码无法推断帧长:已读到 2 个字节,后续字节无法安全界定,
		// 只能判为不支持功能码的畸形帧(不静默丢弃)。
		return nil, &Error{
			Kind: KindUnsupportedFunction,
			Msg:  "rtu: unsupported function code 0x" + hex2(fcByte),
			Raw:  []byte{addr, fcByte},
		}
	}

	// 请求帧长度推断。
	if dir == DirRequest {
		switch fc {
		case FCReadCoils, FCReadDiscreteInputs, FCReadHoldingRegisters, FCReadInputRegisters,
			FCWriteSingleCoil, FCWriteSingleRegister:
			// 地址2 + 数量/值2 + CRC2
			rest, raw, err := pr.readRest(6, []byte{addr, fcByte})
			if err != nil {
				return nil, mapReadErr(err, raw)
			}
			return c.checkAndFrame(addr, fc, append([]byte{fcByte}, rest[:4]...), raw)
		case FCWriteMultipleCoils, FCWriteMultipleRegisters:
			// 地址2 + 数量2 + 字节计数1,随后按字节计数读取载荷 + CRC2。
			head, raw, err := pr.readRest(5, []byte{addr, fcByte})
			if err != nil {
				return nil, mapReadErr(err, raw)
			}
			bc := int(head[4])
			tail, raw2, err := pr.readRest(bc+2, raw)
			if err != nil {
				return nil, mapReadErr(err, raw2)
			}
			pdu := make([]byte, 0, 6+bc)
			pdu = append(pdu, fcByte)
			pdu = append(pdu, head...)
			pdu = append(pdu, tail[:bc]...)
			return c.checkAndFrame(addr, fc, pdu, raw2)
		}
	}

	// 响应帧长度推断。
	switch fc {
	case FCReadCoils, FCReadDiscreteInputs,
		FCReadHoldingRegisters, FCReadInputRegisters:
		// 字节计数1 + 数据n + CRC2。
		bcByte, err := pr.readByte()
		if err != nil {
			return nil, mapReadErr(err, []byte{addr, fcByte})
		}
		bc := int(bcByte)
		data := make([]byte, bc+2)
		if _, err := io.ReadFull(pr, data); err != nil {
			raw := []byte{addr, fcByte, bcByte}
			return nil, mapReadErr(err, append(raw, data...))
		}
		pdu := make([]byte, 0, 2+bc)
		pdu = append(pdu, fcByte, bcByte)
		pdu = append(pdu, data[:bc]...)
		raw := make([]byte, 0, 3+bc+2)
		raw = append(raw, addr, fcByte, bcByte)
		raw = append(raw, data...)
		return c.checkAndFrame(addr, fc, pdu, raw)

	case FCWriteSingleCoil, FCWriteSingleRegister:
		rest, raw, err := pr.readRest(6, []byte{addr, fcByte})
		if err != nil {
			return nil, mapReadErr(err, raw)
		}
		return c.checkAndFrame(addr, fc, append([]byte{fcByte}, rest[:4]...), raw)

	case FCWriteMultipleCoils, FCWriteMultipleRegisters:
		rest, raw, err := pr.readRest(6, []byte{addr, fcByte})
		if err != nil {
			return nil, mapReadErr(err, raw)
		}
		return c.checkAndFrame(addr, fc, append([]byte{fcByte}, rest[:4]...), raw)
	}
	return nil, newError(KindUnsupportedFunction, "rtu: function 0x%s", hex2(fcByte))
}

// checkAndFrame 校验整帧 CRC 并组装统一帧。raw 为含地址、PDU、CRC 的完整字节。
func (c *rtuCodec) checkAndFrame(unit byte, fc FunctionCode, pdu, raw []byte) (*Frame, error) {
	if len(raw) < 3 {
		return nil, &Error{Kind: KindTruncated, Msg: "rtu frame shorter than 3 bytes", Raw: raw}
	}
	gotCRC := uint16(raw[len(raw)-2]) | uint16(raw[len(raw)-1])<<8
	wantCRC := crc16(raw[:len(raw)-2])
	if gotCRC != wantCRC {
		return nil, &Error{
			Kind: KindChecksum,
			Msg:  "rtu crc mismatch: got 0x" + hex4(gotCRC) + ", want 0x" + hex4(wantCRC),
			Raw:  append([]byte(nil), raw...),
		}
	}
	return &Frame{
		Unit: unit,
		PDU:  append([]byte(nil), pdu...),
		Raw:  append([]byte(nil), raw...),
	}, nil
}

// crc16 计算 Modbus CRC16(多项式 0xA001,初始值 0xFFFF)。
func crc16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// peekReader 提供单字节预读式读取,保证推断帧长时不会越界吞掉下一个帧。
type peekReader struct {
	r   io.Reader
	buf [1]byte
}

func newPeekReader(r io.Reader) *peekReader { return &peekReader{r: r} }

func (p *peekReader) Read(b []byte) (int, error) { return p.r.Read(b) }

func (p *peekReader) readByte() (byte, error) {
	_, err := io.ReadFull(p.r, p.buf[:])
	return p.buf[0], err
}

// readRest 从流中再读取 n 个字节,prefix 为已累积的原始字节。
// 返回读到的 n 字节与累积后的完整 raw。
func (p *peekReader) readRest(n int, prefix []byte) ([]byte, []byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(p.r, buf); err != nil {
		return buf, append(append([]byte(nil), prefix...), buf...), err
	}
	raw := make([]byte, 0, len(prefix)+n)
	raw = append(raw, prefix...)
	raw = append(raw, buf...)
	return buf, raw, nil
}

func hex2(b byte) string {
	const hexd = "0123456789ABCDEF"
	return string([]byte{hexd[b>>4], hexd[b&0x0F]})
}

func hex4(v uint16) string {
	return hex2(byte(v>>8)) + hex2(byte(v))
}
