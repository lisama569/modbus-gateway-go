package mapping

import (
	"encoding/binary"
	"math"
)

// EncodeValue 是 DecodeValue 的严格逆过程:按点位数据类型与字节序,把归一化值
// 编码为按地址先后排列的 16 位寄存器序列(每寄存器内为 Modbus 标准大端)。
//
// 值类型必须与数据类型严格对应,不做隐式数值转换,超范围或不兼容一律报错:
//
//	TypeBool      -> bool
//	TypeUint16    -> uint16      TypeInt16   -> int16
//	TypeUint32    -> uint32      TypeInt32   -> int32
//	TypeFloat32   -> float32
//	TypeUint64    -> uint64      TypeInt64   -> int64
//	TypeFloat64   -> float64
//
// 16 位类型只有一个寄存器,不存在字序问题,order 被忽略,固定大端;
// 32/64 位类型按 order 决定字序与字内字节序(与 DecodeValue 完全互逆)。
//
// TypeBool 不占寄存器,本函数对合法 bool 返回空切片(无错误);
// 布尔值由批量写回按线圈位编码下发。
func EncodeValue(t DataType, order ByteOrder, value any) ([]uint16, error) {
	var canonical []byte // 值的标准大端字节 ABCD...
	switch t {
	case TypeBool:
		if _, ok := value.(bool); !ok {
			return nil, valueTypeError(t, value)
		}
		return nil, nil
	case TypeUint16:
		v, ok := value.(uint16)
		if !ok {
			return nil, valueTypeError(t, value)
		}
		canonical = make([]byte, 2)
		binary.BigEndian.PutUint16(canonical, v)
	case TypeInt16:
		v, ok := value.(int16)
		if !ok {
			return nil, valueTypeError(t, value)
		}
		canonical = make([]byte, 2)
		binary.BigEndian.PutUint16(canonical, uint16(v))
	case TypeUint32:
		v, ok := value.(uint32)
		if !ok {
			return nil, valueTypeError(t, value)
		}
		canonical = make([]byte, 4)
		binary.BigEndian.PutUint32(canonical, v)
	case TypeInt32:
		v, ok := value.(int32)
		if !ok {
			return nil, valueTypeError(t, value)
		}
		canonical = make([]byte, 4)
		binary.BigEndian.PutUint32(canonical, uint32(v))
	case TypeFloat32:
		v, ok := value.(float32)
		if !ok {
			return nil, valueTypeError(t, value)
		}
		canonical = make([]byte, 4)
		binary.BigEndian.PutUint32(canonical, math.Float32bits(v))
	case TypeUint64:
		v, ok := value.(uint64)
		if !ok {
			return nil, valueTypeError(t, value)
		}
		canonical = make([]byte, 8)
		binary.BigEndian.PutUint64(canonical, v)
	case TypeInt64:
		v, ok := value.(int64)
		if !ok {
			return nil, valueTypeError(t, value)
		}
		canonical = make([]byte, 8)
		binary.BigEndian.PutUint64(canonical, uint64(v))
	case TypeFloat64:
		v, ok := value.(float64)
		if !ok {
			return nil, valueTypeError(t, value)
		}
		canonical = make([]byte, 8)
		binary.BigEndian.PutUint64(canonical, math.Float64bits(v))
	default:
		return nil, &DecodeError{Msg: "unsupported data type"}
	}

	// reorder 是对合置换:解码时它把线路字节重排为标准大端,
	// 编码时对标准大端做同一置换即得到线路字节。
	wireOrder := order
	if len(canonical) <= 2 {
		wireOrder = BigEndian // 16 位类型固定大端,忽略 order
	}
	wire := reorder(canonical, wireOrder)
	regs := make([]uint16, len(wire)/2)
	for i := range regs {
		regs[i] = binary.BigEndian.Uint16(wire[i*2:])
	}
	return regs, nil
}

// valueTypeError 构造"值类型与点位类型不符"的确定性错误。
func valueTypeError(t DataType, got any) error {
	return &DecodeError{
		Msg: "value type " + goTypeName(got) + " is not compatible with " + dataTypeName(t),
	}
}

func dataTypeName(t DataType) string {
	switch t {
	case TypeBool:
		return "bool"
	case TypeUint16:
		return "uint16"
	case TypeInt16:
		return "int16"
	case TypeUint32:
		return "uint32"
	case TypeInt32:
		return "int32"
	case TypeFloat32:
		return "float32"
	case TypeUint64:
		return "uint64"
	case TypeInt64:
		return "int64"
	case TypeFloat64:
		return "float64"
	}
	return "unknown"
}

func goTypeName(v any) string {
	switch v.(type) {
	case bool:
		return "bool"
	case uint16:
		return "uint16"
	case int16:
		return "int16"
	case uint32:
		return "uint32"
	case int32:
		return "int32"
	case float32:
		return "float32"
	case uint64:
		return "uint64"
	case int64:
		return "int64"
	case float64:
		return "float64"
	case nil:
		return "nil"
	default:
		return "incompatible"
	}
}
