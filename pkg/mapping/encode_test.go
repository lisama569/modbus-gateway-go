package mapping

import (
	"math"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	orders := []ByteOrder{BigEndian, LittleEndian, BigEndianByteSwap, LittleEndianByteSwap}

	// 32 位整数,四种字节序往返
	for _, order := range orders {
		want := uint32(0x12345678)
		regs, err := EncodeValue(TypeUint32, order, want)
		if err != nil {
			t.Fatal(err)
		}
		if len(regs) != 2 {
			t.Fatalf("uint32 wants 2 registers, got %d", len(regs))
		}
		got, err := DecodeValue(TypeUint32, order, regs)
		if err != nil {
			t.Fatal(err)
		}
		if got.(uint32) != want {
			t.Fatalf("uint32 order=%d round-trip: want %08X got %08X", order, want, got)
		}
	}

	// 32 位字节序的确定性线路布局:值 0x12345678
	layouts := map[ByteOrder][]uint16{
		BigEndian:            {0x1234, 0x5678}, // ABCD
		LittleEndian:         {0x7856, 0x3412}, // DCBA
		BigEndianByteSwap:    {0x5678, 0x1234}, // CDAB
		LittleEndianByteSwap: {0x3412, 0x7856}, // BADC
	}
	for order, wantRegs := range layouts {
		regs, err := EncodeValue(TypeUint32, order, uint32(0x12345678))
		if err != nil {
			t.Fatal(err)
		}
		if regs[0] != wantRegs[0] || regs[1] != wantRegs[1] {
			t.Fatalf("order=%d wire layout: want %04X,%04X got %04X,%04X",
				order, wantRegs[0], wantRegs[1], regs[0], regs[1])
		}
	}

	// int32 / float32 往返
	i32regs, err := EncodeValue(TypeInt32, LittleEndianByteSwap, int32(-123456))
	if err != nil {
		t.Fatal(err)
	}
	i32, err := DecodeValue(TypeInt32, LittleEndianByteSwap, i32regs)
	if err != nil || i32.(int32) != -123456 {
		t.Fatalf("int32 round-trip: %v err=%v", i32, err)
	}

	f32regs, err := EncodeValue(TypeFloat32, BigEndian, float32(3.1415927))
	if err != nil {
		t.Fatal(err)
	}
	f32, err := DecodeValue(TypeFloat32, BigEndian, f32regs)
	if err != nil || math.Abs(float64(f32.(float32))-3.1415927) > 1e-6 {
		t.Fatalf("float32 round-trip: %v err=%v", f32, err)
	}
	if f32regs[0] != 0x4049 || f32regs[1] != 0x0FDB {
		t.Fatalf("float32 wire: %04X,%04X", f32regs[0], f32regs[1])
	}

	// 64 位:uint64 / int64 / float64,四种字节序往返
	for _, order := range orders {
		u64 := uint64(0x0102030405060708)
		regs, err := EncodeValue(TypeUint64, order, u64)
		if err != nil {
			t.Fatal(err)
		}
		if len(regs) != 4 {
			t.Fatalf("uint64 wants 4 registers, got %d", len(regs))
		}
		got, err := DecodeValue(TypeUint64, order, regs)
		if err != nil || got.(uint64) != u64 {
			t.Fatalf("uint64 order=%d round-trip: want %016X got %016X err=%v",
				order, u64, got, err)
		}
	}
	i64regs, err := EncodeValue(TypeInt64, BigEndianByteSwap, int64(-9876543210))
	if err != nil {
		t.Fatal(err)
	}
	i64, err := DecodeValue(TypeInt64, BigEndianByteSwap, i64regs)
	if err != nil || i64.(int64) != -9876543210 {
		t.Fatalf("int64 round-trip: %v err=%v", i64, err)
	}
	f64regs, err := EncodeValue(TypeFloat64, LittleEndian, 2.718281828459045)
	if err != nil {
		t.Fatal(err)
	}
	f64, err := DecodeValue(TypeFloat64, LittleEndian, f64regs)
	if err != nil || math.Abs(f64.(float64)-2.718281828459045) > 1e-12 {
		t.Fatalf("float64 round-trip: %v err=%v", f64, err)
	}

	// 16 位:固定大端,order 被忽略
	for _, order := range orders {
		regs, err := EncodeValue(TypeUint16, order, uint16(0xABCD))
		if err != nil || len(regs) != 1 || regs[0] != 0xABCD {
			t.Fatalf("uint16 order=%d: %x err=%v", order, regs, err)
		}
		sregs, err := EncodeValue(TypeInt16, order, int16(-2))
		if err != nil || sregs[0] != 0xFFFE {
			t.Fatalf("int16 order=%d: %x err=%v", order, sregs, err)
		}
	}

	// bool 不产生寄存器
	bregs, err := EncodeValue(TypeBool, BigEndian, true)
	if err != nil || len(bregs) != 0 {
		t.Fatalf("bool encode: regs=%x err=%v", bregs, err)
	}
}

func TestEncodeValueTypeMismatch(t *testing.T) {
	bad := []struct {
		t DataType
		v any
	}{
		{TypeUint16, 42},          // 无类型整型不接受,必须精确 uint16
		{TypeInt16, int32(-1)},    // 宽度不符
		{TypeUint32, uint16(1)},   // 宽度不符
		{TypeFloat32, float64(1)}, // 宽度不符
		{TypeInt64, 1},
		{TypeFloat64, float32(1)},
		{TypeBool, 1},
		{TypeUint32, nil},
	}
	for _, c := range bad {
		if _, err := EncodeValue(c.t, BigEndian, c.v); err == nil {
			t.Fatalf("expected error encoding %v as %s", c.v, dataTypeName(c.t))
		}
	}
}
