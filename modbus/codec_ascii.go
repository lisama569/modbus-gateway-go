package modbus

import (
	"bytes"
	"io"
	"time"
)

const (
	asciiColon = ':'
	asciiCR    = '\r'
	asciiLF    = '\n'
)

// asciiCodec 实现 Modbus ASCII 帧：
// ':' 起始 + 十六进制 ASCII（地址 + PDU + LRC）+ CRLF 结束。
type asciiCodec struct {
	pdu PDU
	gap time.Duration
}

func newASCIICodec() Codec { return &asciiCodec{pdu: NewPDU(), gap: time.Second} }

func (c *asciiCodec) Transport() Transport         { return TransportASCII }
func (c *asciiCodec) InterFrameGap() time.Duration { return c.gap }

func (c *asciiCodec) Encode(adu ADU) ([]byte, error) {
	if len(adu.PDU) == 0 {
		return nil, NewError(KindInvalidArgument, "empty PDU")
	}
	body := make([]byte, 0, 1+len(adu.PDU))
	body = append(body, adu.UnitID)
	body = append(body, adu.PDU...)
	lrc := LRC(body)
	out := make([]byte, 0, 1+2*(len(body)+1)+2)
	out = append(out, asciiColon)
	const hex = "0123456789ABCDEF"
	for _, b := range append(body, lrc) {
		out = append(out, hex[b>>4], hex[b&0x0F])
	}
	out = append(out, asciiCR, asciiLF)
	return out, nil
}

// decodeFrame 解码一帧 ASCII 线路字节（以 ':' 开头、CRLF 结尾），
// 返回 ADU。起始冒号与结尾 CRLF 必须存在，十六进制必须成对，
// LRC 不匹配归类为 KindChecksum。
func (c *asciiCodec) decodeFrame(frame []byte) (ADU, error) {
	if len(frame) < 2+2+2+2 || frame[0] != asciiColon ||
		frame[len(frame)-2] != asciiCR || frame[len(frame)-1] != asciiLF {
		return ADU{}, NewError(KindTruncated, "ASCII frame malformed").WithRaw(frame)
	}
	hexPart := bytes.TrimSpace(frame[1 : len(frame)-2])
	if len(hexPart)%2 != 0 {
		return ADU{}, NewError(KindLengthMismatch,
			"ASCII hex payload must be even chars").WithRaw(frame)
	}
	raw := make([]byte, len(hexPart)/2)
	for i := 0; i < len(raw); i++ {
		hi, ok1 := fromHex(hexPart[2*i])
		lo, ok2 := fromHex(hexPart[2*i+1])
		if !ok1 || !ok2 {
			return ADU{}, NewError(KindInvalidArgument,
				"ASCII frame contains non-hex characters").WithRaw(frame)
		}
		raw[i] = hi<<4 | lo
	}
	if len(raw) < 2 { // 地址 + 至少功能码
		return ADU{}, NewError(KindTruncated, "ASCII frame too short").WithRaw(frame)
	}
	if !CheckLRC(raw[:len(raw)-1], raw[len(raw)-1]) {
		return ADU{}, NewError(KindChecksum, "ASCII LRC mismatch").WithRaw(frame)
	}
	return ADU{UnitID: raw[0], PDU: append([]byte(nil), raw[1:len(raw)-1]...)}, nil
}

func fromHex(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	default:
		return 0, false
	}
}

func (c *asciiCodec) DecodeReader(r io.Reader) (ADU, error) {
	// 丢弃起始冒号之前的噪声字节并定位帧头。
	one := []byte{0}
	for {
		if _, err := io.ReadFull(r, one); err != nil {
			return ADU{}, NewError(KindTruncated, "waiting for ':' failed").WithRaw(nil)
		}
		if one[0] == asciiColon {
			break
		}
	}
	var buf bytes.Buffer
	buf.WriteByte(asciiColon)
	for {
		if _, err := io.ReadFull(r, one); err != nil {
			return ADU{}, NewError(KindTruncated, "ASCII frame truncated before CRLF").
				WithRaw(buf.Bytes())
		}
		buf.WriteByte(one[0])
		if one[0] == asciiLF {
			n := buf.Len()
			if n >= 2 && buf.Bytes()[n-2] == asciiCR {
				return c.decodeFrame(buf.Bytes())
			}
			return ADU{}, NewError(KindLengthMismatch,
				"ASCII frame ended with bare LF").WithRaw(buf.Bytes())
		}
	}
}

func (c *asciiCodec) DecodeBytes(buf []byte) (ADU, []byte, error) {
	start := bytes.IndexByte(buf, asciiColon)
	if start < 0 {
		// 没有帧头：若最后字节是潜在半帧噪声，全部丢弃等待新帧头。
		return ADU{}, nil, nil
	}
	end := bytes.Index(buf[start:], []byte{asciiCR, asciiLF})
	if end < 0 {
		// 帧头之前的噪声丢弃；冒号之后等待更多字节。
		if start > 0 {
			return ADU{}, buf[start:], ErrNeedMore
		}
		return ADU{}, buf, ErrNeedMore
	}
	frameEnd := start + end + 2
	frame := buf[start:frameEnd]
	rest := buf[frameEnd:]
	adu, err := c.decodeFrame(frame)
	return adu, rest, err
}
