package modbus

// FunctionCode 是 Modbus PDU 功能码。
// 本实现统一支持:
//
//	0x01 读线圈          FCReadCoils
//	0x02 读离散输入      FCReadDiscreteInputs
//	0x03 读保持寄存器    FCReadHoldingRegisters
//	0x04 读输入寄存器    FCReadInputRegisters
//	0x05 写单个线圈      FCWriteSingleCoil
//	0x06 写单个寄存器    FCWriteSingleRegister
//	0x0F 写多个线圈      FCWriteMultipleCoils
//	0x10 写多个寄存器    FCWriteMultipleRegisters
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

func (fc FunctionCode) supported() bool {
	switch fc {
	case FCReadCoils, FCReadDiscreteInputs, FCReadHoldingRegisters, FCReadInputRegisters,
		FCWriteSingleCoil, FCWriteSingleRegister, FCWriteMultipleCoils, FCWriteMultipleRegisters:
		return true
	}
	return false
}

// isReadFC 是否为读类功能码。
func isReadFC(fc FunctionCode) bool {
	return fc == FCReadCoils || fc == FCReadDiscreteInputs ||
		fc == FCReadHoldingRegisters || fc == FCReadInputRegisters
}

// isBitFC 是否为位(线圈/离散量)区域功能码。
func isBitFC(fc FunctionCode) bool {
	return fc == FCReadCoils || fc == FCReadDiscreteInputs ||
		fc == FCWriteSingleCoil || fc == FCWriteMultipleCoils
}

// ExceptionCode 是 Modbus 异常码。
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

func (e ExceptionCode) String() string {
	switch e {
	case ExcIllegalFunction:
		return "illegal function"
	case ExcIllegalDataAddress:
		return "illegal data address"
	case ExcIllegalDataValue:
		return "illegal data value"
	case ExcServerDeviceFailure:
		return "server device failure"
	case ExcAcknowledge:
		return "acknowledge"
	case ExcServerDeviceBusy:
		return "server device busy"
	case ExcMemoryParityError:
		return "memory parity error"
	case ExcGatewayPathUnavailable:
		return "gateway target path unavailable"
	case ExcGatewayNoResponse:
		return "gateway target no response"
	default:
		return "unknown exception code"
	}
}

// Request 是所有传输形态(RTU/ASCII/TCP)共用的统一请求模型。
type Request struct {
	// Unit 是从站/单元标识符(RTU: 地址, ASCII: 地址, TCP: Unit ID)。
	Unit byte
	// Function 是功能码。
	Function FunctionCode
	// Address 是起始地址。
	Address uint16
	// Quantity 是读/写数量(位操作以"个"计,寄存器操作以"字"计)。
	Quantity uint16
	// Value 用于 FCWriteSingleCoil(0xFF00/0x0000) 与 FCWriteSingleRegister。
	Value uint16
	// Bytes 是写多个请求中 Quantity 对应的原始载荷。
	Bytes []byte
}

// Response 是所有传输形态共用的统一响应模型。
type Response struct {
	Function FunctionCode
	// Bits 用于读多个线圈/离散输入,每元素为一个位。
	Bits []bool
	// Registers 用于读多个寄存器(FC03/FC04),每元素为一个 16 位寄存器。
	Registers []uint16
	// Address/Value 用于单写回显,Address/Quantity 用于多写回显。
	Address  uint16
	Value    uint16
	Quantity uint16
}

// EncodePDU 把统一请求编码为不带传输封装的 PDU 字节。
func (r *Request) EncodePDU() ([]byte, error) {
	if !r.Function.supported() {
		return nil, newError(KindUnsupportedFunction,
			"function code 0x%02X is not supported", byte(r.Function))
	}
	switch r.Function {
	case FCReadCoils, FCReadDiscreteInputs,
		FCReadHoldingRegisters, FCReadInputRegisters:
		max := uint16(MaxRegistersPerRead)
		if isBitFC(r.Function) {
			max = MaxBitsPerRead
		}
		if r.Quantity == 0 || r.Quantity > max {
			return nil, newError(KindAddress,
				"quantity %d out of range for function 0x%02X", r.Quantity, byte(r.Function))
		}
		pdu := make([]byte, 5)
		pdu[0] = byte(r.Function)
		putUint16(pdu[1:], r.Address)
		putUint16(pdu[3:], r.Quantity)
		return pdu, nil

	case FCWriteSingleCoil:
		if r.Value != 0 && r.Value != 0xFF00 {
			return nil, newError(KindAddress,
				"single coil value must be 0x0000 or 0xFF00, got 0x%04X", r.Value)
		}
		pdu := make([]byte, 5)
		pdu[0] = byte(r.Function)
		putUint16(pdu[1:], r.Address)
		putUint16(pdu[3:], r.Value)
		return pdu, nil

	case FCWriteSingleRegister:
		pdu := make([]byte, 5)
		pdu[0] = byte(r.Function)
		putUint16(pdu[1:], r.Address)
		putUint16(pdu[3:], r.Value)
		return pdu, nil

	case FCWriteMultipleCoils:
		if r.Quantity == 0 || r.Quantity > MaxCoilsPerWrite {
			return nil, newError(KindAddress,
				"coil quantity %d out of range", r.Quantity)
		}
		need := int((r.Quantity + 7) / 8)
		if len(r.Bytes) != need {
			return nil, newError(KindLengthMismatch,
				"coil payload length %d does not match quantity %d (want %d bytes)",
				len(r.Bytes), r.Quantity, need)
		}
		pdu := make([]byte, 0, 6+need)
		pdu = append(pdu, byte(r.Function))
		pdu = appendUint16(pdu, r.Address)
		pdu = appendUint16(pdu, r.Quantity)
		pdu = append(pdu, byte(need))
		pdu = append(pdu, r.Bytes...)
		return pdu, nil

	case FCWriteMultipleRegisters:
		if r.Quantity == 0 || r.Quantity > MaxRegistersPerWrite {
			return nil, newError(KindAddress,
				"register quantity %d out of range", r.Quantity)
		}
		need := int(r.Quantity) * 2
		if len(r.Bytes) != need {
			return nil, newError(KindLengthMismatch,
				"register payload length %d does not match quantity %d (want %d bytes)",
				len(r.Bytes), r.Quantity, need)
		}
		pdu := make([]byte, 0, 6+need)
		pdu = append(pdu, byte(r.Function))
		pdu = appendUint16(pdu, r.Address)
		pdu = appendUint16(pdu, r.Quantity)
		pdu = append(pdu, byte(need))
		pdu = append(pdu, r.Bytes...)
		return pdu, nil
	}
	return nil, newError(KindUnsupportedFunction,
		"function code 0x%02X is not supported", byte(r.Function))
}

// DecodeRequestPDU 从 PDU 字节解码出统一请求(从站侧使用)。
func DecodeRequestPDU(pdu []byte) (*Request, error) {
	if len(pdu) == 0 {
		return nil, newError(KindTruncated, "empty PDU")
	}
	fc := FunctionCode(pdu[0])
	if !fc.supported() {
		return nil, newError(KindUnsupportedFunction,
			"function code 0x%02X is not supported", byte(fc))
	}
	req := &Request{Function: fc}

	switch fc {
	case FCReadCoils, FCReadDiscreteInputs,
		FCReadHoldingRegisters, FCReadInputRegisters,
		FCWriteSingleCoil, FCWriteSingleRegister:
		if len(pdu) < 5 {
			return nil, newError(KindTruncated,
				"function 0x%02X PDU too short: %d bytes, want 5", byte(fc), len(pdu))
		}
		req.Address = Uint16(pdu[1:])
		v := Uint16(pdu[3:])
		if isReadFC(fc) {
			req.Quantity = v
			max := uint16(MaxRegistersPerRead)
			if isBitFC(fc) {
				max = MaxBitsPerRead
			}
			if req.Quantity == 0 || req.Quantity > max {
				return nil, newError(KindAddress,
					"quantity %d out of range for function 0x%02X", req.Quantity, byte(fc))
			}
		} else {
			req.Value = v
		}

	case FCWriteMultipleCoils:
		if len(pdu) < 6 {
			return nil, newError(KindTruncated,
				"function 0x0F PDU too short: %d bytes, want at least 6", len(pdu))
		}
		req.Address = Uint16(pdu[1:])
		req.Quantity = Uint16(pdu[3:])
		bc := int(pdu[5])
		if req.Quantity == 0 || req.Quantity > MaxCoilsPerWrite {
			return nil, newError(KindAddress, "coil quantity %d out of range", req.Quantity)
		}
		if want := int((req.Quantity + 7) / 8); bc != want {
			return nil, newError(KindLengthMismatch,
				"coil byte count %d does not match quantity %d (want %d)", bc, req.Quantity, want)
		}
		if len(pdu) < 6+bc {
			return nil, newError(KindTruncated,
				"coil payload truncated: have %d bytes, want %d", len(pdu)-6, bc)
		}
		req.Bytes = append([]byte(nil), pdu[6:6+bc]...)

	case FCWriteMultipleRegisters:
		if len(pdu) < 6 {
			return nil, newError(KindTruncated,
				"function 0x10 PDU too short: %d bytes, want at least 6", len(pdu))
		}
		req.Address = Uint16(pdu[1:])
		req.Quantity = Uint16(pdu[3:])
		bc := int(pdu[5])
		if req.Quantity == 0 || req.Quantity > MaxRegistersPerWrite {
			return nil, newError(KindAddress, "register quantity %d out of range", req.Quantity)
		}
		if bc != int(req.Quantity)*2 {
			return nil, newError(KindLengthMismatch,
				"register byte count %d does not match quantity %d (want %d)",
				bc, req.Quantity, req.Quantity*2)
		}
		if len(pdu) < 6+bc {
			return nil, newError(KindTruncated,
				"register payload truncated: have %d bytes, want %d", len(pdu)-6, bc)
		}
		req.Bytes = append([]byte(nil), pdu[6:6+bc]...)
	}
	return req, nil
}

// EncodePDU 把统一响应编码为 PDU 字节。
func (r *Response) EncodePDU() ([]byte, error) {
	fc := r.Function
	if !fc.supported() {
		return nil, newError(KindUnsupportedFunction,
			"function code 0x%02X is not supported", byte(fc))
	}
	switch fc {
	case FCReadCoils, FCReadDiscreteInputs:
		bc := (len(r.Bits) + 7) / 8
		pdu := make([]byte, 2+bc)
		pdu[0] = byte(fc)
		pdu[1] = byte(bc)
		for i, b := range r.Bits {
			if b {
				pdu[2+i/8] |= 1 << uint(i%8)
			}
		}
		return pdu, nil

	case FCReadHoldingRegisters, FCReadInputRegisters:
		bc := len(r.Registers) * 2
		pdu := make([]byte, 2+bc)
		pdu[0] = byte(fc)
		pdu[1] = byte(bc)
		for i, v := range r.Registers {
			putUint16(pdu[2+i*2:], v)
		}
		return pdu, nil

	case FCWriteSingleCoil, FCWriteSingleRegister:
		pdu := make([]byte, 5)
		pdu[0] = byte(fc)
		putUint16(pdu[1:], r.Address)
		putUint16(pdu[3:], r.Value)
		return pdu, nil

	case FCWriteMultipleCoils, FCWriteMultipleRegisters:
		pdu := make([]byte, 5)
		pdu[0] = byte(fc)
		putUint16(pdu[1:], r.Address)
		putUint16(pdu[3:], r.Quantity)
		return pdu, nil
	}
	return nil, newError(KindUnsupportedFunction,
		"function code 0x%02X is not supported", byte(fc))
}

// DecodeResponsePDU 依据对应请求的功能码,从 PDU 字节解码统一响应。
// 若 PDU 为 Modbus 异常响应,返回 *Error(Kind == KindExceptionResponse)。
func DecodeResponsePDU(fc FunctionCode, pdu []byte) (*Response, error) {
	if len(pdu) == 0 {
		return nil, newError(KindTruncated, "empty response PDU")
	}
	// 异常响应:功能码最高位置 1。
	if pdu[0]&0x80 != 0 {
		if FunctionCode(pdu[0]&0x7F) != fc {
			return nil, newError(KindTransactionMismatch,
				"exception response function 0x%02X does not match request 0x%02X",
				pdu[0]&0x7F, byte(fc))
		}
		if len(pdu) < 2 {
			return nil, newError(KindTruncated, "exception response missing exception code")
		}
		code := ExceptionCode(pdu[1])
		return nil, &Error{
			Kind:          KindExceptionResponse,
			ExceptionCode: code,
			Msg:           "exception response: " + code.String(),
		}
	}
	if FunctionCode(pdu[0]) != fc {
		return nil, newError(KindTransactionMismatch,
			"response function 0x%02X does not match request 0x%02X", pdu[0], byte(fc))
	}

	resp := &Response{Function: fc}
	switch fc {
	case FCReadCoils, FCReadDiscreteInputs:
		if len(pdu) < 2 {
			return nil, newError(KindTruncated, "bit read response missing byte count")
		}
		bc := int(pdu[1])
		if len(pdu) < 2+bc {
			return nil, newError(KindTruncated,
				"bit read payload truncated: have %d bytes, want %d", len(pdu)-2, bc)
		}
		resp.Bits = make([]bool, bc*8)
		for i := 0; i < bc*8; i++ {
			resp.Bits[i] = pdu[2+i/8]&(1<<uint(i%8)) != 0
		}

	case FCReadHoldingRegisters, FCReadInputRegisters:
		if len(pdu) < 2 {
			return nil, newError(KindTruncated, "register read response missing byte count")
		}
		bc := int(pdu[1])
		if bc%2 != 0 {
			return nil, newError(KindLengthMismatch,
				"register read byte count %d is not even", bc)
		}
		if len(pdu) < 2+bc {
			return nil, newError(KindTruncated,
				"register read payload truncated: have %d bytes, want %d", len(pdu)-2, bc)
		}
		resp.Registers = make([]uint16, bc/2)
		for i := range resp.Registers {
			resp.Registers[i] = Uint16(pdu[2+i*2:])
		}

	case FCWriteSingleCoil, FCWriteSingleRegister:
		if len(pdu) < 5 {
			return nil, newError(KindTruncated,
				"single-write echo too short: %d bytes, want 5", len(pdu))
		}
		resp.Address = Uint16(pdu[1:])
		resp.Value = Uint16(pdu[3:])

	case FCWriteMultipleCoils, FCWriteMultipleRegisters:
		if len(pdu) < 5 {
			return nil, newError(KindTruncated,
				"multiple-write echo too short: %d bytes, want 5", len(pdu))
		}
		resp.Address = Uint16(pdu[1:])
		resp.Quantity = Uint16(pdu[3:])
	}
	return resp, nil
}

// ExceptionResponsePDU 生成异常响应 PDU(功能码 | 0x80 + 异常码)。
func ExceptionResponsePDU(fc FunctionCode, code ExceptionCode) []byte {
	return []byte{byte(fc) | 0x80, byte(code)}
}

func putUint16(b []byte, v uint16) {
	b[0] = byte(v >> 8)
	b[1] = byte(v)
}

func appendUint16(b []byte, v uint16) []byte {
	return append(b, byte(v>>8), byte(v))
}

// Uint16 从大端字节读取 uint16。
func Uint16(b []byte) uint16 {
	return uint16(b[0])<<8 | uint16(b[1])
}
