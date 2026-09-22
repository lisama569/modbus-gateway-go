package mapping

import (
	"encoding/binary"
	"math"
)

// Size 返回该数据类型占用的寄存器数(位类型返回 1,位宽在读位区域时单独处理)。
func (t DataType) Size() uint16 {
	switch t {
	case TypeBool, TypeUint16, TypeInt16:
		return 1
	case TypeUint32, TypeInt32, TypeFloat32:
		return 2
	case TypeUint64, TypeInt64, TypeFloat64:
		return 4
	}
	return 1
}

// DecodeValue 按数据类型与字节序,把原始寄存器序列确定地解码为归一化值。
//
// 寄存器序列按地址先后从设备读出。四种字节序采用 Modbus 社区通用的
// ABCD 记法(A 为值最高字节,D 为最低字节):
//
//	BigEndian             ABCD  字不交换、字内字节不交换(Modbus 标准大端)
//	LittleEndian          DCBA  字交换、字内字节交换(标准小端)
//	BigEndianByteSwap     CDAB  字交换、字内字节不交换(常称"字交换")
//	LittleEndianByteSwap  BADC  字不交换、字内字节交换(常称"字节交换")
//
// 16 位类型只有一个寄存器,不存在字序问题,始终按大端解析(Modbus 规范)。
func DecodeValue(t DataType, order ByteOrder, regs []uint16) (any, error) {
	raw := encodeRegisters(BigEndian, regs) // 线性大端字节:ABCD...
	switch t {
	case TypeBool:
		return raw[0] != 0, nil
	case TypeUint16:
		if err := checkWidth(raw, 2); err != nil {
			return nil, err
		}
		return binary.BigEndian.Uint16(raw), nil
	case TypeInt16:
		if err := checkWidth(raw, 2); err != nil {
			return nil, err
		}
		return int16(binary.BigEndian.Uint16(raw)), nil
	case TypeUint32:
		if err := checkWidth(raw, 4); err != nil {
			return nil, err
		}
		return binary.BigEndian.Uint32(reorder(raw, order)), nil
	case TypeInt32:
		if err := checkWidth(raw, 4); err != nil {
			return nil, err
		}
		return int32(binary.BigEndian.Uint32(reorder(raw, order))), nil
	case TypeFloat32:
		if err := checkWidth(raw, 4); err != nil {
			return nil, err
		}
		return math.Float32frombits(binary.BigEndian.Uint32(reorder(raw, order))), nil
	case TypeUint64:
		if err := checkWidth(raw, 8); err != nil {
			return nil, err
		}
		return binary.BigEndian.Uint64(reorder(raw, order)), nil
	case TypeInt64:
		if err := checkWidth(raw, 8); err != nil {
			return nil, err
		}
		return int64(binary.BigEndian.Uint64(reorder(raw, order))), nil
	case TypeFloat64:
		if err := checkWidth(raw, 8); err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(reorder(raw, order))), nil
	}
	return nil, &DecodeError{Msg: "unsupported data type"}
}

// checkWidth 校验原始字节长度恰好为 want。
func checkWidth(raw []byte, want int) error {
	if len(raw) != want {
		return &DecodeError{Msg: "register width mismatch: have " +
			itoaLenM(len(raw)) + " bytes, want " + itoaLenM(want)}
	}
	return nil
}

// reorder 把线性大端字节按字节序/字序重排为"大端可读"字节。
func reorder(raw []byte, order ByteOrder) []byte {
	n := len(raw)
	out := make([]byte, n)
	words := n / 2
	for w := 0; w < words; w++ {
		hi, lo := raw[w*2], raw[w*2+1] // 大端:hi 在前
		var dst int
		var swap bool
		switch order {
		case BigEndian: // ABCD: 不换字,不换字节
			dst, swap = w, false
		case LittleEndian: // DCBA: 换字且换字节
			dst, swap = words-1-w, true
		case BigEndianByteSwap: // CDAB: 换字,不换字节
			dst, swap = words-1-w, false
		case LittleEndianByteSwap: // BADC: 不换字,换字节
			dst, swap = w, true
		}
		if swap {
			out[dst*2], out[dst*2+1] = lo, hi
		} else {
			out[dst*2], out[dst*2+1] = hi, lo
		}
	}
	return out
}

// DecodeError 是数据类型兼容转换错误。
type DecodeError struct {
	Msg string
}

func (e *DecodeError) Error() string { return "mapping decode: " + e.Msg }

// encodeRegisters 把 regs 按字内大端展开为连续字节序列(寄存器按地址先后)。
func encodeRegisters(_ ByteOrder, regs []uint16) []byte {
	buf := make([]byte, len(regs)*2)
	for i, r := range regs {
		binary.BigEndian.PutUint16(buf[i*2:], r)
	}
	return buf
}

func itoaLenM(n int) string {
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
