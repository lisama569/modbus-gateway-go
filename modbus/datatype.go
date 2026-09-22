package modbus

import (
	"encoding/binary"
	"fmt"
	"math"
)

// DataType 支持的点位标量类型（寄存器区使用）。
type DataType int

const (
	TypeUint16 DataType = iota
	TypeInt16
	TypeUint32
	TypeInt32
	TypeFloat32
	TypeUint64
	TypeInt64
	TypeFloat64
)

func (t DataType) String() string {
	switch t {
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
	default:
		return "unknown"
	}
}

// ByteWordOrder 以“线路字节排列标签”确定地描述字节序与字序组合。
//
// 约定：把线路上的字节依次按 1,2,3,4（32 位）或 1..8（64 位）编号，
// 标签描述“自然大端表示中的字节如何排列到线路上”。
// 以 0x12345678（自然字节 MSB->LSB 为 12 34 56 78）为例：
//
//	ABCD : 12 34 56 78   大端、字序正向（标准 Modbus 32 位）
//	DCBA : 78 56 34 12   小端、字序反向（全小端）
//	BADC : 34 12 78 56   大端字、字节内交换（字交换+字节交换）
//	CDAB : 56 78 12 34   字交换（常见“字序翻转”设备）
//
// 64 位标签 ABCDEFGH / HGFEDCBA / GHEFCDAB / BADCFEHG 等遵循同一规则。
type ByteWordOrder string

const (
	OrderABCD ByteWordOrder = "ABCD"
	OrderBADC ByteWordOrder = "BADC"
	OrderCDAB ByteWordOrder = "CDAB"
	OrderDCBA ByteWordOrder = "DCBA"

	OrderABCDEFGH ByteWordOrder = "ABCDEFGH"
	OrderHGFEDCBA ByteWordOrder = "HGFEDCBA"
	OrderGHEFCDAB ByteWordOrder = "GHEFCDAB"
	OrderBADCFEHG ByteWordOrder = "BADCFEHG"
)

// Size 返回该类型占用的寄存器（16 位字）数量。
func (t DataType) Size() int {
	switch t {
	case TypeUint16, TypeInt16:
		return 1
	case TypeUint32, TypeInt32, TypeFloat32:
		return 2
	case TypeUint64, TypeInt64, TypeFloat64:
		return 4
	default:
		return 0
	}
}

// permutation 把标签解释为“线路位置 i 上的字节，在自然大端表示中的索引”。
// 例如 ABCD -> [0,1,2,3]，CDAB -> [2,3,0,1]。
func permutation(order ByteWordOrder) ([]int, error) {
	n := len(order)
	if n != 4 && n != 8 {
		return nil, NewError(KindInvalidArgument,
			"byte/word order label must be 4 (32-bit) or 8 (64-bit) letters: "+string(order))
	}
	perm := make([]int, n)
	seen := make([]bool, n)
	for i, ch := range order {
		var idx int
		switch {
		case ch >= 'A' && ch <= 'H':
			idx = int(ch - 'A')
		case ch >= 'a' && ch <= 'h':
			idx = int(ch - 'a')
		default:
			return nil, NewError(KindInvalidArgument, "bad order letter: "+string(ch))
		}
		if idx >= n || seen[idx] {
			return nil, NewError(KindInvalidArgument,
				"order label must be a permutation of A.."+string(rune('A'+n-1)))
		}
		seen[idx] = true
		perm[i] = idx
	}
	return perm, nil
}

// wordsToWireBytes 把寄存器大端字序列展平为线路字节。
func wordsToWireBytes(words []uint16) []byte {
	b := make([]byte, 2*len(words))
	for i, w := range words {
		binary.BigEndian.PutUint16(b[2*i:], w)
	}
	return b
}

// DecodeRaw 按指定排列把寄存器大端字序列还原为自然大端字节表示并转成 uint64 位模式。
func DecodeRaw(t DataType, order ByteWordOrder, words []uint16) (uint64, error) {
	size := t.Size()
	if size == 0 {
		return 0, NewError(KindInvalidArgument, "unknown data type")
	}
	if len(words) != size {
		return 0, NewError(KindInvalidArgument,
			fmt.Sprintf("%s needs %d words, got %d", t, size, len(words)))
	}
	wire := wordsToWireBytes(words)
	natural := make([]byte, len(wire))
	if len(wire) == 2 {
		// 16 位类型不存在字序问题，仅整体大端。
		natural = wire
	} else {
		perm, err := permutation(order)
		if err != nil {
			return 0, err
		}
		if len(perm) != len(wire) {
			return 0, NewError(KindInvalidArgument,
				fmt.Sprintf("order %s applies to %d bytes but type occupies %d", order, len(perm), len(wire)))
		}
		// perm[i] 是线路第 i 字节在自然表示中的位置。
		for i := range wire {
			natural[perm[i]] = wire[i]
		}
	}
	var bits uint64
	for _, b := range natural {
		bits = bits<<8 | uint64(b)
	}
	return bits, nil
}

// DecodeValue 解码为 float64 标量（整数按精确值返回，浮点按其值返回）。
func DecodeValue(t DataType, order ByteWordOrder, words []uint16) (float64, error) {
	bits, err := DecodeRaw(t, order, words)
	if err != nil {
		return 0, err
	}
	switch t {
	case TypeUint16:
		return float64(uint16(bits)), nil
	case TypeInt16:
		return float64(int16(bits)), nil
	case TypeUint32:
		return float64(uint32(bits)), nil
	case TypeInt32:
		return float64(int32(bits)), nil
	case TypeFloat32:
		return float64(math.Float32frombits(uint32(bits))), nil
	case TypeUint64:
		return float64(bits), nil // 超过 2^53 会有精度损失，原始位模式可用 DecodeRaw
	case TypeInt64:
		return float64(int64(bits)), nil
	case TypeFloat64:
		return math.Float64frombits(bits), nil
	default:
		return 0, NewError(KindInvalidArgument, "unknown data type")
	}
}

// EncodeValue 把 float64 标量按类型与排列编码为寄存器大端字序列。
func EncodeValue(t DataType, order ByteWordOrder, value float64) ([]uint16, error) {
	var bits uint64
	switch t {
	case TypeUint16:
		if value < 0 || value > 0xFFFF {
			return nil, NewError(KindInvalidArgument, "uint16 out of range")
		}
		bits = uint64(uint16(value))
	case TypeInt16:
		if value < -32768 || value > 32767 {
			return nil, NewError(KindInvalidArgument, "int16 out of range")
		}
		bits = uint64(int16(value))
	case TypeUint32:
		if value < 0 || value > 0xFFFFFFFF {
			return nil, NewError(KindInvalidArgument, "uint32 out of range")
		}
		bits = uint64(uint32(value))
	case TypeInt32:
		if value < -2147483648 || value > 2147483647 {
			return nil, NewError(KindInvalidArgument, "int32 out of range")
		}
		bits = uint64(int32(value))
	case TypeFloat32:
		bits = uint64(math.Float32bits(float32(value)))
	case TypeUint64:
		bits = uint64(value)
	case TypeInt64:
		bits = uint64(int64(value))
	case TypeFloat64:
		bits = math.Float64bits(value)
	default:
		return nil, NewError(KindInvalidArgument, "unknown data type")
	}

	size := t.Size()
	natural := make([]byte, 2*size)
	switch size {
	case 1:
		binary.BigEndian.PutUint16(natural, uint16(bits))
	case 2:
		binary.BigEndian.PutUint32(natural, uint32(bits))
	case 4:
		binary.BigEndian.PutUint64(natural, bits)
	}
	if size == 1 {
		return []uint16{binary.BigEndian.Uint16(natural)}, nil
	}
	perm, err := permutation(order)
	if err != nil {
		return nil, err
	}
	if len(perm) != len(natural) {
		return nil, NewError(KindInvalidArgument, "order width disagrees with type")
	}
	wire := make([]byte, len(natural))
	for i := range natural {
		wire[i] = natural[perm[i]]
	}
	words := make([]uint16, size)
	for i := range words {
		words[i] = binary.BigEndian.Uint16(wire[2*i:])
	}
	return words, nil
}
