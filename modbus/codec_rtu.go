package modbus

import (
	"fmt"
	"io"
	"time"
)

const rtuMinFrame = 4 // 地址(1) + 功能码(1) + CRC(2)

// rtuCodec 实现 Modbus RTU 帧（地址 + PDU + CRC16，小端 CRC）。
// 本地模拟时承载在 TCP 连接之上：按功能码推断帧长完成定界，
// 并以帧间静默时长区分帧边界。
type rtuCodec struct {
	pdu PDU
	gap time.Duration
}

func newRTUCodec() Codec { return &rtuCodec{pdu: NewPDU(), gap: 3500 * time.Microsecond} }

func (c *rtuCodec) Transport() Transport         { return TransportRTU }
func (c *rtuCodec) InterFrameGap() time.Duration { return c.gap }

func (c *rtuCodec) Encode(adu ADU) ([]byte, error) {
	if len(adu.PDU) == 0 {
		return nil, NewError(KindInvalidArgument, "empty PDU")
	}
	frame := make([]byte, 0, len(adu.PDU)+3)
	frame = append(frame, adu.UnitID)
	frame = append(frame, adu.PDU...)
	return AppendCRC(frame), nil
}

// rtuPDULen 依据功能码推断“地址之后、CRC 之前”的 PDU 长度；
// 读/写多请求与读响应由字节计数字段决定；返回 0 表示无法识别。
func rtuPDULen(pdu []byte, isRequest bool) int {
	if len(pdu) < 2 {
		return -1 // 连功能码都不完整 -> 截断
	}
	fc := FunctionCode(pdu[0])
	if fc&exceptionBit != 0 {
		return 2 // 异常响应固定 2 字节
	}
	switch fc {
	case FCReadCoils, FCReadDiscreteInputs, FCReadHoldingRegisters, FCReadInputRegisters:
		if isRequest {
			return 5
		}
		// 响应：FC + bytecount + data
		if len(pdu) < 2 {
			return -1
		}
		return 2 + int(pdu[1])
	case FCWriteSingleCoil, FCWriteSingleRegister:
		return 5 // 请求与响应均为 5 字节
	case FCWriteMultipleCoils, FCWriteMultipleRegisters:
		if isRequest {
			if len(pdu) < 6 {
				return -1
			}
			return 6 + int(pdu[5])
		}
		return 5 // 响应固定回显 4 字节
	default:
		return 0 // 不支持的功能码，无法定界
	}
}

// DecodeReader 读取一个完整 RTU 帧。
func (c *rtuCodec) DecodeReader(r io.Reader) (ADU, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(r, head); err != nil {
		return ADU{}, NewError(KindTruncated, "RTU frame shorter than addr+fc").WithRaw(head)
	}
	fc := FunctionCode(head[1])
	if _, supported := supportedFC(fc); !supported && fc&exceptionBit == 0 {
		return ADU{}, NewError(KindUnsupportedFunction,
			fmt.Sprintf("function code 0x%02X not supported", byte(fc))).WithRaw(head)
	}
	// 通过缓冲推断，按功能码确定的长度读取整帧。
	return c.readByInference(r, head)
}

// supportedFC 报告功能码是否为受支持的标准码。
func supportedFC(fc FunctionCode) (FunctionCode, bool) {
	switch fc {
	case FCReadCoils, FCReadDiscreteInputs, FCReadHoldingRegisters, FCReadInputRegisters,
		FCWriteSingleCoil, FCWriteSingleRegister, FCWriteMultipleCoils, FCWriteMultipleRegisters:
		return fc, true
	default:
		return fc, false
	}
}

// readByInference 通过缓冲推断读取完整帧（含 CRC）。
func (c *rtuCodec) readByInference(r io.Reader, head []byte) (ADU, error) {
	buf := make([]byte, 0, 32)
	buf = append(buf, head...)
	fc := FunctionCode(head[1])
	isRequest := inferRequestDirection(fc, head)
	// 读到能推断长度为止。
	for {
		pdu := buf[1:]
		n := rtuPDULen(pdu, isRequest)
		if n > 0 {
			total := 1 + n + 2 // addr + pdu + crc
			if len(buf) < total {
				more := make([]byte, total-len(buf))
				if _, err := io.ReadFull(r, more); err != nil {
					return ADU{}, NewError(KindTruncated, "RTU frame truncated per inferred length").
						WithRaw(append(buf, more...))
				}
				buf = append(buf, more...)
			}
			return c.checkAndADU(buf)
		}
		if n == 0 {
			return ADU{}, NewError(KindUnsupportedFunction,
				fmt.Sprintf("function code 0x%02X cannot be framed", byte(fc))).WithRaw(buf)
		}
		one := []byte{0}
		if _, err := io.ReadFull(r, one); err != nil {
			return ADU{}, NewError(KindTruncated, "RTU frame truncated while inferring length").
				WithRaw(buf)
		}
		buf = append(buf, one[0])
	}
}

// inferRequestDirection 仅靠字节无法 100% 判断方向，
// 读类功能码第二字节 < 2 表示响应的 byte count 起始位置——
// 长度推断对请求(5)与响应(bytecount)区分：此处由调用上下文决定；
// 默认按响应字节计数规则（主站收到的是响应），服务端使用 DecodeBytes 时传 isRequest。
func inferRequestDirection(fc FunctionCode, head []byte) bool {
	return false
}

func (c *rtuCodec) checkAndADU(frame []byte) (ADU, error) {
	if !CheckCRC(frame) {
		return ADU{}, NewError(KindChecksum, "RTU CRC16 mismatch").WithRaw(frame)
	}
	return ADU{UnitID: frame[0], PDU: append([]byte(nil), frame[1:len(frame)-2]...)}, nil
}

// DecodeBytes 从缓冲解析一帧。isRequest 方向无法从字节判断，
// 帧长度推断同时兼容请求与响应两种布局，取首个 CRC 合法的解释。
func (c *rtuCodec) DecodeBytes(buf []byte) (ADU, []byte, error) {
	if len(buf) < rtuMinFrame {
		return ADU{}, buf, ErrNeedMore
	}
	fc := FunctionCode(buf[1])
	if _, ok := supportedFC(fc); !ok && fc&exceptionBit == 0 {
		// 功能码无法识别：无法可靠定界，整段判损坏。
		return ADU{}, nil, NewError(KindUnsupportedFunction,
			fmt.Sprintf("function code 0x%02X not supported", byte(fc))).WithRaw(buf)
	}
	// 依次尝试两种方向的长度推断。
	for _, isReq := range []bool{false, true} {
		n := rtuPDULen(buf[1:], isReq)
		if n <= 0 {
			continue
		}
		total := 1 + n + 2
		if len(buf) < total {
			continue
		}
		frame := buf[:total]
		if CheckCRC(frame) {
			adu := ADU{UnitID: frame[0], PDU: append([]byte(nil), frame[1:total-2]...)}
			return adu, buf[total:], nil
		}
	}
	// 两种解释都未构成合法帧：只要有一种定长解释已收齐字节但 CRC 失败，
	// 即判定校验错误；全部解释都缺字节才等待更多数据。
	nResp := rtuPDULen(buf[1:], false)
	nReq := rtuPDULen(buf[1:], true)
	needMore := true
	for _, n := range []int{nResp, nReq} {
		if n > 0 && len(buf) >= 1+n+2 {
			needMore = false // 该解释已收齐但 CRC 不匹配
		}
	}
	if needMore {
		return ADU{}, buf, ErrNeedMore
	}
	return ADU{}, nil, NewError(KindChecksum, "RTU CRC16 mismatch").WithRaw(buf)
}
