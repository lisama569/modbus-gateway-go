package modbus

import "fmt"

// ErrorKind 是对 Modbus 通信过程中所有可区分错误的分类。
type ErrorKind int

const (
	KindUnknown ErrorKind = iota
	// KindTruncated 帧长度不足 / 截断
	KindTruncated
	// KindLengthMismatch 长度字段与实际载荷长度不一致
	KindLengthMismatch
	// KindChecksum CRC/LRC 校验失败
	KindChecksum
	// KindTransactionMismatch 事务标识(或单元标识符)不匹配
	KindTransactionMismatch
	// KindUnsupportedFunction 主站不支持或从站返回非法功能码
	KindUnsupportedFunction
	// KindExceptionResponse 从站返回 Modbus 异常响应
	KindExceptionResponse
	// KindTimeout 请求超时
	KindTimeout
	// KindClosed 连接已关闭 / 不可用
	KindClosed
	// KindAddress 寄存器地址或数量非法
	KindAddress
	// KindPartial 批量读取部分点位失败
	KindPartial
)

// Error 携带错误分类、归一化原因以及触发判定的原始报文。
type Error struct {
	Kind ErrorKind
	// Msg 归一化后的人类可读判定结果
	Msg string
	// Raw 触发该判定的原始报文(可能为 nil)
	Raw []byte
	// ExceptionCode 在 Kind == KindExceptionResponse 时有效
	ExceptionCode ExceptionCode
	// Cause 包装的底层错误
	Cause error
}

func (e *Error) Error() string {
	if e.Msg != "" {
		return "modbus: " + e.Msg
	}
	return "modbus: " + kindText(e.Kind)
}

func (e *Error) Unwrap() error { return e.Cause }

// KindOf 返回任意错误对应的 ErrorKind,未知错误归为 KindUnknown。
func KindOf(err error) ErrorKind {
	for err != nil {
		if e, ok := err.(*Error); ok {
			return e.Kind
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return KindUnknown
}

func kindText(k ErrorKind) string {
	switch k {
	case KindTruncated:
		return "truncated frame"
	case KindLengthMismatch:
		return "length mismatch"
	case KindChecksum:
		return "checksum failure"
	case KindTransactionMismatch:
		return "transaction mismatch"
	case KindUnsupportedFunction:
		return "unsupported function code"
	case KindExceptionResponse:
		return "exception response"
	case KindTimeout:
		return "request timeout"
	case KindClosed:
		return "connection closed"
	case KindAddress:
		return "invalid address or quantity"
	case KindPartial:
		return "partial read failure"
	default:
		return "unknown error"
	}
}

func newError(kind ErrorKind, format string, args ...any) *Error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}
