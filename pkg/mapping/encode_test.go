package mapping

import (
	"math"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	orders := []ByteOrder{BigEndian, LittleEndian, BigEndianByteSwap, LittleEndianByteSwap}

	cases := []struct {
		typ   DataType
		value any
	}{
		{TypeUint16, uint16(0xABCD)},
		{TypeInt16, int16(-1234)},
		{TypeUint32, uint32(0x12345678)},
		{TypeInt32, int32(-0x123456)},
		{TypeFloat32, float32(3.1415927)},
		{TypeUint64, uint64(0x0102030405060708)},
		{TypeInt64, int64(-1 << 40)},
		{TypeFloat64, 2.718281828459045},
	}
	for _, c := range cases {
		for _, o := range orders {
			regs, err := EncodeValue(c.typ, o, c.value)
			if err != nil {
				t.Fatalf("encode %v order=%d: %v", c.typ, o, err)
			}
			if len(regs) != int(c.typ.Size()) {
				t.Fatalf("encode %v: got %d registers, want %d",
					c.typ, len(regs), c.typ.Size())
			}
			back, err := DecodeValue(c.typ, o, regs)
			if err != nil {
				t.Fatalf("decode %v order=%d: %v", c.typ, o, err)
			}
			switch want := c.value.(type) {
			case float32:
				if math.Abs(float64(back.(float32))-float64(want)) > 1e-6 {
					t.Fatalf("%v order=%d: got %v want %v", c.typ, o, back, want)
				}
			case float64:
				if math.Abs(back.(float64)-want) > 1e-12 {
					t.Fatalf("%v order=%d: got %v want %v", c.typ, o, back, want)
				}
			default:
				if back != c.value {
					t.Fatalf("%v order=%d: got %v want %v", c.typ, o, back, c.value)
				}
			}
		}
	}
}

func TestEncodeKnownLayouts(t *testing.T) {
	// 32 位 0x12345678 在四种字节序下的线路寄存器布局(地址先后)。
	want := map[ByteOrder][]uint16{
		BigEndian:            {0x1234, 0x5678}, // ABCD
		LittleEndian:         {0x7856, 0x3412}, // DCBA
		BigEndianByteSwap:    {0x5678, 0x1234}, // CDAB
		LittleEndianByteSwap: {0x3412, 0x7856}, // BADC
	}
	for o, w := range want {
		regs, err := EncodeValue(TypeUint32, o, uint32(0x12345678))
		if err != nil {
			t.Fatal(err)
		}
		if len(regs) != 2 || regs[0] != w[0] || regs[1] != w[1] {
			t.Fatalf("order=%d: regs=%04x,%04x want %04x,%04x", o, regs[0], regs[1], w[0], w[1])
		}
	}

	// 16 位类型忽略字节序
	regs, err := EncodeValue(TypeUint16, LittleEndian, uint16(0xABCD))
	if err != nil || regs[0] != 0xABCD {
		t.Fatalf("uint16 encode: %v %v", regs, err)
	}

	// 64 位大端
	r64, err := EncodeValue(TypeUint64, BigEndian, uint64(0x0102030405060708))
	if err != nil {
		t.Fatal(err)
	}
	if r64[0] != 0x0102 || r64[3] != 0x0708 {
		t.Fatalf("uint64 BE = %04x...", r64)
	}
}

func TestEncodeValueRejections(t *testing.T) {
	bad := []struct {
		typ   DataType
		value any
	}{
		{TypeBool, true},         // 布尔必须走线圈
		{TypeUint16, 42},         // int 不是 uint16
		{TypeUint16, int16(42)},  // 宽度不混用
		{TypeInt32, int64(42)},   // 宽度不混用
		{TypeUint32, uint16(42)}, // 宽度不混用
		{TypeFloat32, 1.5},       // untyped float64 不等于 float32
		{TypeFloat32, float32(math.NaN())},
		{TypeFloat64, math.Inf(1)},
		{TypeInt64, uint64(1)},
		{DataType(99), uint16(1)},
	}
	for _, b := range bad {
		if _, err := EncodeValue(b.typ, BigEndian, b.value); err == nil {
			t.Fatalf("expected error for %v <- %T", b.typ, b.value)
		}
	}
}
