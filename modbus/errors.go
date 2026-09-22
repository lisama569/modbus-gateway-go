package modbus

import (
	"errors"
	"fmt"
)

// ErrorKind 错误分类：所有失败都必须归类到以下互斥的类别之一，
// 不允许静默丢弃或把异常响应当作正常数据。
type ErrorKind int

const (
	KindOK                  ErrorKind = iota
	KindTruncated                     // 报文截断 / 长度不足
	KindLengthMismatch                // 长度字段与实际字节数不一致
	KindChecksum                      // CRC / LRC 校验失败
	KindTransactionMismatch           // 事务标识 / 单元标识不匹配
	KindUnsupportedFunction           // 功能码不支持
	KindModbusException               // Modbus 异常响应
	KindTimeout                       // 请求超时
	KindConnClosed                    // 连接失效
	KindInvalidArgument               // 请求参数非法
	KindPartialFailure                // 批量读取部分点位失败
)

// String 返回错误类别的稳定短名，用于日志与测试断言。
func (k ErrorKind) String() string {
	switch k {
	case KindOK:
		return "ok"
	case KindTruncated:
		return "truncated"
	case KindLengthMismatch:
		return "length_mismatch"
	case KindChecksum:
		return "checksum"
	case KindTransactionMismatch:
		return "transaction_mismatch"
	case KindUnsupportedFunction:
		return "unsupported_function"
	case KindModbusException:
		return "modbus_exception"
	case KindTimeout:
		return "timeout"
	case KindConnClosed:
		return "connection_closed"
	case KindInvalidArgument:
		return "invalid_argument"
	case KindPartialFailure:
		return "partial_failure"
	default:
		return fmt.Sprintf("unknown(%d)", int(k))
	}
}

// ExceptionCode Modbus 标准异常码（功能码最高位置 1 时携带）。
type ExceptionCode byte

const (
	ExcIllegalFunction        ExceptionCode = 0x01
	ExcIllegalDataAddress     ExceptionCode = 0x02
	ExcIllegalDataValue       ExceptionCode = 0x03
	ExcServerDeviceFailure    ExceptionCode = 0x04
	ExcAcknowledge            ExceptionCode = 0x05
	ExcServerDeviceBusy       ExceptionCode = 0x06
	ExcMemoryParityError      ExceptionCode = 0x08
	ExcGatewayPathUnavailable ExceptionCode = 0x0A
	ExcGatewayNoResponse      ExceptionCode = 0x0B
)

// String 返回异常码的标准名称。
func (x ExceptionCode) String() string {
	switch x {
	case ExcIllegalFunction:
		return "illegal_function"
	case ExcIllegalDataAddress:
		return "illegal_data_address"
	case ExcIllegalDataValue:
		return "illegal_data_value"
	case ExcServerDeviceFailure:
		return "server_device_failure"
	case ExcAcknowledge:
		return "acknowledge"
	case ExcServerDeviceBusy:
		return "server_device_busy"
	case ExcMemoryParityError:
		return "memory_parity_error"
	case ExcGatewayPathUnavailable:
		return "gateway_path_unavailable"
	case ExcGatewayNoResponse:
		return "gateway_no_response"
	default:
		return fmt.Sprintf("unknown_exception(0x%02X)", byte(x))
	}
}

// Error 是全模块统一的错误类型，携带可区分的错误类别、
// 可选的 Modbus 异常码、触发错误的原始报文与解释性说明。
type Error struct {
	Kind    ErrorKind
	Except  ExceptionCode // 仅 Kind == KindModbusException 时有意义
	Raw     []byte        // 触发错误的原始报文（可空）
	Message string
	Cause   error
}

func (e *Error) Error() string {
	s := "modbus: " + e.Kind.String()
	if e.Kind == KindModbusException {
		s += ":" + e.Except.String()
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	if len(e.Raw) > 0 {
		s += fmt.Sprintf(" [raw=% x]", e.Raw)
	}
	return s
}

func (e *Error) Unwrap() error { return e.Cause }

// NewError 构造分类错误。
func NewError(kind ErrorKind, msg string) *Error {
	return &Error{Kind: kind, Message: msg}
}

// WrapError 包装底层错误并分类。
func WrapError(kind ErrorKind, msg string, cause error) *Error {
	return &Error{Kind: kind, Message: msg, Cause: cause}
}

// ExceptionError 从异常功能码与异常码构造错误，raw 为原始异常 PDU/帧。
func ExceptionError(reqFC FunctionCode, code ExceptionCode, raw []byte) *Error {
	return &Error{
		Kind:    KindModbusException,
		Except:  code,
		Raw:     raw,
		Message: fmt.Sprintf("function 0x%02X raised exception %s", byte(reqFC), code.String()),
	}
}

// WithRaw 附加原始报文后返回同一错误（便于链式构造）。
func (e *Error) WithRaw(raw []byte) *Error {
	e.Raw = append(e.Raw[:0:0], raw...)
	return e
}

// AsError 尝试把任意错误归一化为 *Error；不是则返回 nil。
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var me *Error
	if errors.As(err, &me) {
		return me
	}
	return nil
}

// Retryable 判断该错误是否属于可重试类别。
// 语义：仅超时、连接失效、帧长度不匹配会在会话层触发“重连 + 重试”；
// 校验失败、事务不匹配、异常响应等均为确定结论，绝不重试，
// 以免把同一写请求重复提交到设备。
func (e *Error) Retryable() bool {
	switch e.Kind {
	case KindTimeout, KindConnClosed, KindLengthMismatch:
		return true
	default:
		return false
	}
}
