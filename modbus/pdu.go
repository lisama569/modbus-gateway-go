package modbus

import (
	"encoding/binary"
	"fmt"
)

// FunctionCode 标准 Modbus 功能码。
type FunctionCode byte

const (
	FCReadCoils              FunctionCode = 0x01
	FCReadDiscreteInputs     FunctionCode = 0x02
	FCReadHoldingRegisters   FunctionCode = 0x03
	FCReadInputRegisters     FunctionCode = 0x04
	FCWriteSingleCoil        FunctionCode = 0x05
	FCWriteSingleRegister    FunctionCode = 0x06
	FCWriteMultipleCoils     FunctionCode = 0x0F
	FCWriteMultipleRegisters FunctionCode = 0x10
)

// exceptionBit 标记异常响应：功能码最高位置 1。
const exceptionBit = 0x80

func (f FunctionCode) String() string {
	switch f {
	case FCReadCoils:
		return "read_coils(0x01)"
	case FCReadDiscreteInputs:
		return "read_discrete_inputs(0x02)"
	case FCReadHoldingRegisters:
		return "read_holding_registers(0x03)"
	case FCReadInputRegisters:
		return "read_input_registers(0x04)"
	case FCWriteSingleCoil:
		return "write_single_coil(0x05)"
	case FCWriteSingleRegister:
		return "write_single_register(0x06)"
	case FCWriteMultipleCoils:
		return "write_multiple_coils(0x0F)"
	case FCWriteMultipleRegisters:
		return "write_multiple_registers(0x10)"
	default:
		return fmt.Sprintf("unknown(0x%02X)", byte(f))
	}
}

// Region 是 Modbus 四类数据区的归一化表示。
type Region int

const (
	RegionCoils Region = iota
	RegionDiscreteInputs
	RegionHoldingRegisters
	RegionInputRegisters
)

func (r Region) String() string {
	switch r {
	case RegionCoils:
		return "coils"
	case RegionDiscreteInputs:
		return "discrete_inputs"
	case RegionHoldingRegisters:
		return "holding_registers"
	case RegionInputRegisters:
		return "input_registers"
	default:
		return "unknown_region"
	}
}

// IsRegister 报告该区域是否为 16 位寄存器区。
func (r Region) IsRegister() bool {
	return r == RegionHoldingRegisters || r == RegionInputRegisters
}

// ReadFC 返回读取该区域使用的标准功能码。
func (r Region) ReadFC() FunctionCode {
	switch r {
	case RegionCoils:
		return FCReadCoils
	case RegionDiscreteInputs:
		return FCReadDiscreteInputs
	case RegionHoldingRegisters:
		return FCReadHoldingRegisters
	case RegionInputRegisters:
		return FCReadInputRegisters
	default:
		return 0
	}
}

// 协议常量。
const (
	MaxBitsPerReadPDU       = 2000
	MaxRegistersPerReadPDU  = 125
	MaxCoilsPerWritePDU     = 1968 // 246 字节载荷 * 8
	MaxRegistersPerWritePDU = 123  // PDU 253 字节限制
)

// Request 是与传输形态无关的统一请求模型。
type Request struct {
	FC       FunctionCode
	Address  uint16
	Quantity uint16 // 读/批量写：点数或寄存器数；单写：忽略
	Values   []byte // 批量写载荷；单写使用前 2 字节（线圈 0x0000/0xFF00）
	UnitID   byte   // 单元 / 从站标识（由帧层写入 ADU，PDU 编解码忽略）
}

// Response 是与传输形态无关的统一响应模型。
type Response struct {
	FC      FunctionCode
	Data    []byte // 读取：寄存器/线圈数据；其余可为空
	Written uint16
	Address uint16
}

// PDU 是 Modbus PDU 编解码器：功能码 + 数据，不含任何传输封装。
type PDU interface {
	EncodeRequest(req Request) ([]byte, error)
	EncodeResponse(resp Response) ([]byte, error)
	DecodeRequest(pdu []byte) (Request, error)
	DecodeResponse(pdu []byte, reqFC FunctionCode) (Response, error)
}

// stdPDU 标准 Modbus PDU 编解码。
type stdPDU struct{}

// NewPDU 返回标准协议 PDU 编解码器。
func NewPDU() PDU { return stdPDU{} }

func putBE16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }

func be16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }

// EncodeRequest 把统一请求编码为 PDU（功能码 + 数据）。
func (stdPDU) EncodeRequest(req Request) ([]byte, error) {
	switch req.FC {
	case FCReadCoils, FCReadDiscreteInputs, FCReadHoldingRegisters, FCReadInputRegisters:
		if req.Quantity == 0 {
			return nil, NewError(KindInvalidArgument, "quantity must be > 0")
		}
		limit := uint16(MaxBitsPerReadPDU)
		if req.FC >= FCReadHoldingRegisters {
			limit = MaxRegistersPerReadPDU
		}
		if req.Quantity > limit {
			return nil, NewError(KindInvalidArgument,
				fmt.Sprintf("quantity %d exceeds PDU limit %d for %s", req.Quantity, limit, req.FC))
		}
		pdu := make([]byte, 5)
		pdu[0] = byte(req.FC)
		putBE16(pdu[1:3], req.Address)
		putBE16(pdu[3:5], req.Quantity)
		return pdu, nil
	case FCWriteSingleCoil:
		if len(req.Values) < 2 {
			return nil, NewError(KindInvalidArgument, "write single coil needs 2-byte value")
		}
		v := be16(req.Values[:2])
		if v != 0 && v != 0xFF00 {
			return nil, NewError(KindInvalidArgument,
				fmt.Sprintf("illegal coil value 0x%04X (want 0x0000/0xFF00)", v))
		}
		pdu := make([]byte, 5)
		pdu[0] = byte(req.FC)
		putBE16(pdu[1:3], req.Address)
		putBE16(pdu[3:5], v)
		return pdu, nil
	case FCWriteSingleRegister:
		if len(req.Values) < 2 {
			return nil, NewError(KindInvalidArgument, "write single register needs 2-byte value")
		}
		pdu := make([]byte, 5)
		pdu[0] = byte(req.FC)
		putBE16(pdu[1:3], req.Address)
		copy(pdu[3:5], req.Values[:2])
		return pdu, nil
	case FCWriteMultipleCoils:
		if req.Quantity == 0 || req.Quantity > MaxCoilsPerWritePDU {
			return nil, NewError(KindInvalidArgument, "illegal coil write quantity")
		}
		need := int((req.Quantity + 7) / 8)
		if len(req.Values) < need {
			return nil, NewError(KindInvalidArgument, "coil payload too short")
		}
		pdu := make([]byte, 6+need)
		pdu[0] = byte(req.FC)
		putBE16(pdu[1:3], req.Address)
		putBE16(pdu[3:5], req.Quantity)
		pdu[5] = byte(need)
		copy(pdu[6:], req.Values[:need])
		return pdu, nil
	case FCWriteMultipleRegisters:
		if req.Quantity == 0 || req.Quantity > MaxRegistersPerWritePDU {
			return nil, NewError(KindInvalidArgument, "illegal register write quantity")
		}
		need := int(req.Quantity) * 2
		if len(req.Values) < need {
			return nil, NewError(KindInvalidArgument, "register payload too short")
		}
		pdu := make([]byte, 6+need)
		pdu[0] = byte(req.FC)
		putBE16(pdu[1:3], req.Address)
		putBE16(pdu[3:5], req.Quantity)
		pdu[5] = byte(need)
		copy(pdu[6:], req.Values[:need])
		return pdu, nil
	default:
		return nil, NewError(KindUnsupportedFunction,
			fmt.Sprintf("function code 0x%02X not supported", byte(req.FC)))
	}
}

// EncodeResponse 把统一响应编码为 PDU。
func (p stdPDU) EncodeResponse(resp Response) ([]byte, error) {
	switch resp.FC {
	case FCReadCoils, FCReadDiscreteInputs:
		pdu := make([]byte, 2+len(resp.Data))
		pdu[0] = byte(resp.FC)
		pdu[1] = byte(len(resp.Data))
		copy(pdu[2:], resp.Data)
		return pdu, nil
	case FCReadHoldingRegisters, FCReadInputRegisters:
		if len(resp.Data)%2 != 0 {
			return nil, NewError(KindInvalidArgument, "register data must be even bytes")
		}
		pdu := make([]byte, 2+len(resp.Data))
		pdu[0] = byte(resp.FC)
		pdu[1] = byte(len(resp.Data))
		copy(pdu[2:], resp.Data)
		return pdu, nil
	case FCWriteSingleCoil, FCWriteSingleRegister:
		pdu := make([]byte, 5)
		pdu[0] = byte(resp.FC)
		putBE16(pdu[1:3], resp.Address)
		putBE16(pdu[3:5], resp.Written) // 单写响应回显值
		return pdu, nil
	case FCWriteMultipleCoils, FCWriteMultipleRegisters:
		pdu := make([]byte, 5)
		pdu[0] = byte(resp.FC)
		putBE16(pdu[1:3], resp.Address)
		putBE16(pdu[3:5], resp.Written)
		return pdu, nil
	default:
		return nil, NewError(KindUnsupportedFunction,
			fmt.Sprintf("function code 0x%02X not supported", byte(resp.FC)))
	}
}

// EncodeException 编码异常响应 PDU（异常功能码 + 异常码）。
func EncodeException(reqFC FunctionCode, code ExceptionCode) []byte {
	return []byte{byte(reqFC) | exceptionBit, byte(code)}
}

// DecodeRequest 从 PDU 解码统一请求（服务端使用）。
func (p stdPDU) DecodeRequest(pdu []byte) (Request, error) {
	var req Request
	if len(pdu) < 1 {
		return req, NewError(KindTruncated, "empty PDU").WithRaw(pdu)
	}
	fc := FunctionCode(pdu[0])
	switch fc {
	case FCReadCoils, FCReadDiscreteInputs, FCReadHoldingRegisters, FCReadInputRegisters:
		if len(pdu) != 5 {
			return req, NewError(KindLengthMismatch,
				fmt.Sprintf("%s request wants 5 bytes, got %d", fc, len(pdu))).WithRaw(pdu)
		}
		req.FC, req.Address, req.Quantity = fc, be16(pdu[1:3]), be16(pdu[3:5])
		return req, nil
	case FCWriteSingleCoil, FCWriteSingleRegister:
		if len(pdu) != 5 {
			return req, NewError(KindLengthMismatch,
				fmt.Sprintf("%s request wants 5 bytes, got %d", fc, len(pdu))).WithRaw(pdu)
		}
		req.FC, req.Address = fc, be16(pdu[1:3])
		req.Values = pdu[3:5]
		return req, nil
	case FCWriteMultipleCoils, FCWriteMultipleRegisters:
		if len(pdu) < 6 {
			return req, NewError(KindTruncated,
				fmt.Sprintf("%s request too short", fc)).WithRaw(pdu)
		}
		req.FC, req.Address = fc, be16(pdu[1:3])
		req.Quantity = be16(pdu[3:5])
		bc := int(pdu[5])
		if len(pdu) != 6+bc {
			return req, NewError(KindLengthMismatch,
				fmt.Sprintf("%s byte count %d disagrees with PDU length %d", fc, bc, len(pdu)-6)).
				WithRaw(pdu)
		}
		req.Values = pdu[6:]
		return req, nil
	default:
		return req, NewError(KindUnsupportedFunction,
			fmt.Sprintf("function code 0x%02X not supported", byte(fc))).WithRaw(pdu)
	}
}

// DecodeResponse 结合请求功能码解码响应 PDU。
// 异常位、截断、长度不一致、功能码不匹配都会被识别为可区分错误。
func (p stdPDU) DecodeResponse(pdu []byte, reqFC FunctionCode) (Response, error) {
	var resp Response
	if len(pdu) < 2 {
		return resp, NewError(KindTruncated, "response PDU shorter than 2 bytes").WithRaw(pdu)
	}
	// 异常响应优先识别，绝不作为正常数据返回。
	if pdu[0]&exceptionBit != 0 {
		gotFC := FunctionCode(pdu[0] &^ exceptionBit)
		if gotFC != reqFC {
			return resp, NewError(KindTransactionMismatch,
				fmt.Sprintf("exception function 0x%02X does not match request 0x%02X", gotFC, reqFC)).
				WithRaw(pdu)
		}
		if len(pdu) != 2 {
			return resp, NewError(KindLengthMismatch,
				"exception response must be exactly 2 bytes").WithRaw(pdu)
		}
		return resp, ExceptionError(reqFC, ExceptionCode(pdu[1]), pdu)
	}

	fc := FunctionCode(pdu[0])
	if fc != reqFC {
		return resp, NewError(KindTransactionMismatch,
			fmt.Sprintf("response function 0x%02X does not match request 0x%02X", fc, reqFC)).
			WithRaw(pdu)
	}
	resp.FC = fc
	switch fc {
	case FCReadCoils, FCReadDiscreteInputs:
		bc := int(pdu[1])
		if len(pdu) != 2+bc {
			return resp, NewError(KindLengthMismatch,
				fmt.Sprintf("byte count %d disagrees with PDU length %d", bc, len(pdu)-2)).
				WithRaw(pdu)
		}
		resp.Data = append([]byte(nil), pdu[2:]...)
		return resp, nil
	case FCReadHoldingRegisters, FCReadInputRegisters:
		bc := int(pdu[1])
		if bc%2 != 0 {
			return resp, NewError(KindLengthMismatch,
				"register byte count must be even").WithRaw(pdu)
		}
		if len(pdu) != 2+bc {
			return resp, NewError(KindLengthMismatch,
				fmt.Sprintf("byte count %d disagrees with PDU length %d", bc, len(pdu)-2)).
				WithRaw(pdu)
		}
		resp.Data = append([]byte(nil), pdu[2:]...)
		return resp, nil
	case FCWriteSingleCoil, FCWriteSingleRegister:
		if len(pdu) != 5 {
			return resp, NewError(KindLengthMismatch,
				fmt.Sprintf("write response wants 5 bytes, got %d", len(pdu))).WithRaw(pdu)
		}
		resp.Address = be16(pdu[1:3])
		resp.Written = be16(pdu[3:5])
		return resp, nil
	case FCWriteMultipleCoils, FCWriteMultipleRegisters:
		if len(pdu) != 5 {
			return resp, NewError(KindLengthMismatch,
				fmt.Sprintf("write-multiple response wants 5 bytes, got %d", len(pdu))).WithRaw(pdu)
		}
		resp.Address = be16(pdu[1:3])
		resp.Written = be16(pdu[3:5])
		return resp, nil
	default:
		return resp, NewError(KindUnsupportedFunction,
			fmt.Sprintf("function code 0x%02X not supported", fc)).WithRaw(pdu)
	}
}
