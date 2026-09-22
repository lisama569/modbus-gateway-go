package modbus

import (
	"errors"
	"io"
	"net"
	"os"
	"time"
)

// Frame 是穿过传输层的一帧:统一单元标识 + PDU + 原始字节。
type Frame struct {
	Unit byte
	PDU  []byte
	// Raw 是该帧在线路上的完整原始字节(RTU 含 CRC,ASCII 含起止符,
	// TCP 含 MBAP),用于日志与错误判定追溯。
	Raw []byte
	// TransactionID 仅 TCP 形态有效。
	TransactionID uint16
}

// Link 是面向一帧收发的传输抽象。
// 本地环境下不同形态都承载在本地流之上(见 server.go 中的本地监听器),
// 由 Codec 负责各自的分帧与校验语义,从而保证上层请求/响应与失败判定一致。
type Link interface {
	io.Reader
	io.Writer
	io.Closer
	SetDeadline(t time.Time) error
}

// Codec 负责某种传输形态的分帧、校验与编解码。
// dir 表示要解码的帧方向(请求或响应),因为 RTU/ASCII 这类无长度字段
// 的形态必须依据功能码推断响应帧长度。
type Codec interface {
	// Encode 编码一帧到字节。
	Encode(f *Frame) ([]byte, error)
	// Decode 从 r 中读取并解码一帧。
	Decode(r io.Reader, dir Direction) (*Frame, error)
	// Name 返回传输形态名称(tcp/rtu/ascii)。
	Name() string
}

// Direction 表示帧的解码方向。
type Direction int

const (
	DirRequest Direction = iota
	DirResponse
)

// Transport 标识传输形态。
type Transport string

const (
	TransportTCP   Transport = "tcp"
	TransportRTU   Transport = "rtu"
	TransportASCII Transport = "ascii"
)

// NewCodec 按传输形态创建对应的分帧编解码器。
func NewCodec(t Transport) (Codec, error) {
	switch t {
	case TransportTCP:
		return &tcpCodec{}, nil
	case TransportRTU:
		return &rtuCodec{}, nil
	case TransportASCII:
		return &asciiCodec{}, nil
	default:
		return nil, newError(KindUnsupportedFunction,
			"unknown transport %q (supported: tcp, rtu, ascii)", string(t))
	}
}

// 帧内单次请求数量上限(Modbus 规范)。
const (
	MaxBitsPerRead       = 2000
	MaxRegistersPerRead  = 125
	MaxCoilsPerWrite     = 1968
	MaxRegistersPerWrite = 123
)

// mapReadErr 把读帧过程中的底层错误归一化到网关错误分类:
// 超时 -> KindTimeout;连接干净关闭 -> KindClosed;读到部分字节后断开/截断 -> KindTruncated。
func mapReadErr(err error, raw []byte) *Error {
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return &Error{Kind: KindTimeout, Msg: "deadline exceeded while reading frame", Raw: raw, Cause: err}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &Error{Kind: KindTimeout, Msg: "i/o timeout while reading frame", Raw: raw, Cause: err}
	}
	if errors.Is(err, io.EOF) {
		return &Error{Kind: KindClosed, Msg: "connection closed by peer", Raw: raw, Cause: err}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return &Error{Kind: KindTruncated, Msg: "frame truncated: connection ended mid-frame", Raw: raw, Cause: err}
	}
	return &Error{Kind: KindClosed, Msg: "read failed: " + err.Error(), Raw: raw, Cause: err}
}

// mapBodyReadErr 用于 TCP 这类带显式长度字段的形态读取 PDU 体时的分类。
// 与 mapReadErr 的关键区别:若声明了长度 declared,而在超时/断开前确实收到了
// 一部分体字节(partialLen > 0)但收不满,则判定为"长度字段与实际载荷不一致",
// 而不是笼统的超时/关闭 —— 从而使"声明长度大于实际响应"这类帧错误可与
// "对端完全不响应(丢弃)"稳定区分。
func mapBodyReadErr(err error, raw []byte, declared, partialLen int) *Error {
	if err == nil {
		return nil
	}
	isTimeout := errors.Is(err, os.ErrDeadlineExceeded)
	var ne net.Error
	if !isTimeout && errors.As(err, &ne) && ne.Timeout() {
		isTimeout = true
	}
	if partialLen > 0 && partialLen < declared {
		kind := KindLengthMismatch
		msg := "declared length " + itoaN(declared) + " exceeds actual payload (" +
			itoaN(partialLen) + " byte(s) received)"
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			msg = "declared length " + itoaN(declared) + " but connection ended after " +
				itoaN(partialLen) + " byte(s)"
		}
		return &Error{Kind: kind, Msg: msg, Raw: raw, Cause: err}
	}
	return mapReadErr(err, raw)
}

func itoaN(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
