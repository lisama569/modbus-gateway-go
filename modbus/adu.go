package modbus

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"
)

// ADU 是应用数据单元：传输帧解码后的统一视图。
// Meta 携带传输形态相关的附加信息（TCP 的事务标识），会话层用于匹配。
type ADU struct {
	UnitID byte
	PDU    []byte
	TxnID  uint16 // 仅 TCP MBAP 事务标识有意义；RTU/ASCII 为 0
}

// Codec 抽象三种传输形态（TCP / RTU / ASCII）共同的帧收发。
type Codec interface {
	// Encode 把 ADU 编码为线路字节。TxnID 仅 TCP 使用。
	Encode(adu ADU) ([]byte, error)
	// DecodeReader 从流中读取并解码一个完整帧。
	DecodeReader(r io.Reader) (ADU, error)
	// DecodeBytes 从缓冲字节解码一帧，返回剩余字节；
	// 缓冲不足时返回 ErrNeedMore（包装为 KindTruncated 由会话层判定）。
	DecodeBytes(buf []byte) (adu ADU, rest []byte, err error)
	// InterFrameGap 返回 RTU/ASCII 建议的帧间静默时长，TCP 返回 0。
	InterFrameGap() time.Duration
	// Transport 返回该编解码器对应的传输形态标识。
	Transport() Transport
}

// ErrNeedMore 表示当前缓冲不足以组成完整帧（帧内截断由 KindTruncated 表示）。
var ErrNeedMore = errors.New("modbus: need more bytes for a complete frame")

// Transport 标识传输形态。
type Transport string

const (
	TransportTCP   Transport = "tcp"   // Modbus TCP（MBAP + TCP）
	TransportRTU   Transport = "rtu"   // Modbus RTU over TCP（本地模拟串口链路）
	TransportASCII Transport = "ascii" // Modbus ASCII over TCP（本地模拟串口链路）
)

// NewCodec 按传输形态构造帧编解码器。
func NewCodec(t Transport) (Codec, error) {
	switch t {
	case TransportTCP:
		return newTCPCodec(), nil
	case TransportRTU:
		return newRTUCodec(), nil
	case TransportASCII:
		return newASCIICodec(), nil
	default:
		return nil, NewError(KindInvalidArgument, "unknown transport: "+string(t))
	}
}

// FrameTransport 是 Codec 与底层连接的组合，供会话层使用。
type FrameTransport interface {
	io.Closer
	Send(ctx context.Context, adu ADU) error
	Recv(ctx context.Context) (ADU, error)
	Transport() Transport
}

// frameTransport 串行化同一连接上的帧读写（会话层另保证请求串行）。
type frameTransport struct {
	conn    net.Conn
	codec   Codec
	readBuf []byte // 已读但尚未组成完整帧的字节
}

// NewFrameTransport 用已有连接与编解码器组装帧传输。
func NewFrameTransport(conn net.Conn, codec Codec) FrameTransport {
	return &frameTransport{conn: conn, codec: codec}
}

func (f *frameTransport) Transport() Transport { return f.codec.Transport() }

// Send 编码并写出一帧。
func (f *frameTransport) Send(ctx context.Context, adu ADU) error {
	wire, err := f.codec.Encode(adu)
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = f.conn.SetWriteDeadline(dl)
	}
	if _, err := f.conn.Write(wire); err != nil {
		return WrapError(KindConnClosed, "write frame failed", err)
	}
	return nil
}

// Recv 读取并解码一帧。截断/损坏帧由 codec 返回分类错误；
// 读失败统一归类为连接失效，以便会话层触发重连。
func (f *frameTransport) Recv(ctx context.Context) (ADU, error) {
	if dl, ok := ctx.Deadline(); ok {
		_ = f.conn.SetReadDeadline(dl)
	}
	// 先尝试消费缓冲区里已有的完整帧。
	for {
		if len(f.readBuf) > 0 {
			adu, rest, derr := f.codec.DecodeBytes(f.readBuf)
			if derr == nil {
				f.readBuf = append(f.readBuf[:0:0], rest...)
				return adu, nil
			}
			if !errors.Is(derr, ErrNeedMore) {
				// 已判定为损坏帧：丢弃该帧的字节后返回错误，避免污染下一帧。
				f.readBuf = f.discardBadFrame(f.readBuf)
				return ADU{}, derr
			}
		}
		tmp := make([]byte, 256)
		n, err := f.conn.Read(tmp)
		if n > 0 {
			f.readBuf = append(f.readBuf, tmp[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return ADU{}, WrapError(KindConnClosed, "connection closed by peer", err)
			}
			// 超时由会话层根据 ctx 判定；其他读错误归连接失效。
			if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
				return ADU{}, NewError(KindTimeout, "read deadline exceeded")
			}
			return ADU{}, WrapError(KindConnClosed, "read frame failed", err)
		}
	}
}

// discardBadFrame 在损坏帧后尽量恢复缓冲：TCP 按 MBAP 长度字段跳过整个坏帧，
// RTU/ASCII 无法可靠定界则清空缓冲（后续依赖事务标识继续检测错配）。
func (f *frameTransport) discardBadFrame(buf []byte) []byte {
	if f.codec.Transport() == TransportTCP && len(buf) >= mbapHeaderSize {
		length := int(binary.BigEndian.Uint16(buf[4:6]))
		if length >= 1 {
			total := mbapHeaderSize + length - 1
			if total <= len(buf) {
				return append([]byte(nil), buf[total:]...)
			}
		}
	}
	return nil
}

func (f *frameTransport) Close() error { return f.conn.Close() }
