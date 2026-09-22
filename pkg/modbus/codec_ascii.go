package modbus

import (
	"bytes"
	"encoding/hex"
	"io"
)

// asciiCodec 实现 Modbus ASCII 分帧。
//
// 帧布局(可见 ASCII 字符):
//
//	':' (0x3A) | 地址(2 hex) | PDU(2n hex) | LRC(2 hex) | CR LF
//
// LRC 覆盖"地址 + PDU",按二补码计算。每两个 hex 字符代表一个字节。
type asciiCodec struct{}

func (c *asciiCodec) Name() string { return string(TransportASCII) }

func (c *asciiCodec) Encode(f *Frame) ([]byte, error) {
	payload := make([]byte, 0, len(f.PDU)+1)
	payload = append(payload, f.Unit)
	payload = append(payload, f.PDU...)
	lrc := lrc8(payload)

	var buf bytes.Buffer
	buf.Grow(1 + (len(payload)+1)*2 + 2)
	buf.WriteByte(':')
	h := hex.NewEncoder(&buf)
	h.Write(payload)
	h.Write([]byte{lrc})
	buf.WriteString("\r\n")
	raw := buf.Bytes()
	f.Raw = raw
	return raw, nil
}

func (c *asciiCodec) Decode(r io.Reader, dir Direction) (*Frame, error) {
	// 读取起始符 ':'。流中可能出现噪声字节,这里以冒号为唯一帧定界。
	first := make([]byte, 1)
	for {
		if _, err := io.ReadFull(r, first); err != nil {
			return nil, mapReadErr(err, nil)
		}
		if first[0] == ':' {
			break
		}
	}

	// 读到 CRLF 为止,收集所有 hex 字符(限制最大长度防止无界读取)。
	var body bytes.Buffer
	line := make([]byte, 1)
	crSeen := false
	const maxBody = 520 // (1+252+1) 字节 * 2
	for {
		if _, err := io.ReadFull(r, line); err != nil {
			raw := append([]byte{':'}, body.Bytes()...)
			return nil, mapReadErr(err, raw)
		}
		b := line[0]
		if crSeen {
			if b != '\n' {
				return nil, &Error{
					Kind: KindTruncated,
					Msg:  "ascii frame: expected LF after CR, got 0x" + hex2(b),
					Raw:  append([]byte{':'}, body.Bytes()...),
				}
			}
			break
		}
		if b == '\r' {
			crSeen = true
			continue
		}
		if body.Len() >= maxBody {
			return nil, &Error{
				Kind: KindLengthMismatch,
				Msg:  "ascii frame exceeds maximum encoded length",
				Raw:  append([]byte{':'}, body.Bytes()...),
			}
		}
		body.WriteByte(b)
	}

	raw := append([]byte{':'}, body.Bytes()...)
	raw = append(raw, '\r', '\n')

	decoded, err := hex.DecodeString(body.String())
	if err != nil {
		return nil, &Error{
			Kind: KindChecksum,
			Msg:  "ascii frame contains invalid hexadecimal characters: " + err.Error(),
			Raw:  raw,
		}
	}
	if len(decoded) < 3 { // 地址 + 至少功能码 + LRC
		return nil, &Error{
			Kind: KindTruncated,
			Msg:  "ascii frame too short: decoded length " + itoaLen(len(decoded)),
			Raw:  raw,
		}
	}

	content := decoded[:len(decoded)-1]
	gotLRC := decoded[len(decoded)-1]
	wantLRC := lrc8(content)
	if gotLRC != wantLRC {
		return nil, &Error{
			Kind: KindChecksum,
			Msg:  "ascii lrc mismatch: got 0x" + hex2(gotLRC) + ", want 0x" + hex2(wantLRC),
			Raw:  raw,
		}
	}

	return &Frame{
		Unit: content[0],
		PDU:  append([]byte(nil), content[1:]...),
		Raw:  raw,
	}, nil
}

// lrc8 计算 Modbus ASCII 的纵向冗余校验(二补码和)。
func lrc8(data []byte) byte {
	sum := 0
	for _, b := range data {
		sum += int(b)
	}
	return byte(-int8(sum & 0xFF))
}

func itoaLen(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
