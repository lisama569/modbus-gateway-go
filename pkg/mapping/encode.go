package mapping

import (
	"encoding/binary"
	"fmt"
	"math"
)

// EncodeValue 是 DecodeValue 的逆过程:按数据类型与字节序把归一化值
// 编码为"按地址先后排列"的寄存器序列。
//
// 仅用于可写保持寄存器;线圈的开关量由写批次层直接处理(TypeBool 在此报错)。
// 值的 Go 类型必须与点位类型严格对应(int/uint 宽度不混用、float 宽度不混用),
// 超出该类型表示范围同样报错,绝不静默截断或回绕。
//
// 字节序语义与 DecodeValue 完全对称(reorder 为自逆置换):
//
//	BigEndian             ABCD
//	LittleEndian          DCBA
//	BigEndianByteSwap     CDAB
//	LittleEndianByteSwap  BADC
//
// 16 位类型只有一个寄存器,固定按 Modbus 大端编码。
func EncodeValue(t DataType, order ByteOrder, value any) ([]uint16, error) {
	var canon []byte // 值的规范大端字节
	switch t {
	case TypeBool:
		return nil, &DecodeError{Msg: "bool values are encoded as coils, not registers"}
	case TypeUint16:
		v, ok := value.(uint16)
		if !ok {
			return nil, typeMismatch(t, value)
		}
		canon = be16(v)
	case TypeInt16:
		v, ok := value.(int16)
		if !ok {
			return nil, typeMismatch(t, value)
		}
		canon = be16(uint16(v))
	case TypeUint32:
		v, ok := value.(uint32)
		if !ok {
			return nil, typeMismatch(t, value)
		}
		canon = be32(v)
	case TypeInt32:
		v, ok := value.(int32)
		if !ok {
			return nil, typeMismatch(t, value)
		}
		canon = be32(uint32(v))
	case TypeFloat32:
		v, ok := value.(float32)
		if !ok {
			return nil, typeMismatch(t, value)
		}
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, &DecodeError{Msg: "float32 value must be finite"}
		}
		canon = be32(math.Float32bits(v))
	case TypeUint64:
		v, ok := value.(uint64)
		if !ok {
			return nil, typeMismatch(t, value)
		}
		canon = be64(v)
	case TypeInt64:
		v, ok := value.(int64)
		if !ok {
			return nil, typeMismatch(t, value)
		}
		canon = be64(uint64(v))
	case TypeFloat64:
		v, ok := value.(float64)
		if !ok {
			return nil, typeMismatch(t, value)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, &DecodeError{Msg: "float64 value must be finite"}
		}
		canon = be64(math.Float64bits(v))
	default:
		return nil, &DecodeError{Msg: "unsupported data type"}
	}

	// reorder 对任意字节序都是自逆置换,因此编码与解码共用同一映射。
	// 16 位类型只有一个字,不存在字/字节序问题(与 DecodeValue 一致),忽略 order。
	wire := canon
	if len(canon) > 2 {
		wire = reorder(canon, order)
	}
	regs := make([]uint16, len(wire)/2)
	for i := range regs {
		regs[i] = binary.BigEndian.Uint16(wire[i*2:])
	}
	return regs, nil
}

func be16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func be32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func be64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func typeMismatch(t DataType, value any) error {
	return &DecodeError{Msg: fmt.Sprintf(
		"value type %T does not match point data type %d", value, int(t))}
}
