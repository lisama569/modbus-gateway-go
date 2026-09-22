package modbus

import (
	"context"
	"math"
	"net"
	"testing"
	"time"
)

func TestDataTypeRoundTrip(t *testing.T) {
	cases := []struct {
		typ   DataType
		order ByteWordOrder
		val   float64
	}{
		{TypeUint16, "", 0xBEEF},
		{TypeInt16, "", -1234},
		{TypeUint32, OrderABCD, 0x12345678},
		{TypeUint32, OrderDCBA, 0x12345678},
		{TypeUint32, OrderBADC, 0x12345678},
		{TypeUint32, OrderCDAB, 0x12345678},
		{TypeInt32, OrderABCD, -2000000000},
		{TypeFloat32, OrderCDAB, 3.14159},
		{TypeFloat64, OrderABCDEFGH, math.Pi},
		{TypeUint64, OrderHGFEDCBA, 0x0102030405060708},
	}
	for _, tc := range cases {
		words, err := EncodeValue(tc.typ, tc.order, tc.val)
		if err != nil {
			t.Fatalf("%v %s encode: %v", tc.typ, tc.order, err)
		}
		got, err := DecodeValue(tc.typ, tc.order, words)
		if err != nil {
			t.Fatalf("%v %s decode: %v", tc.typ, tc.order, err)
		}
		if math.Abs(got-tc.val) > 1e-4*math.Max(1, math.Abs(tc.val)) {
			t.Fatalf("%v %s roundtrip: want %v got %v", tc.typ, tc.order, tc.val, got)
		}
		t.Logf("[%s/%s] words=% x -> value=%v verdict=ok", tc.typ, tc.order, words, got)
	}
}

func TestByteOrderDeterministic(t *testing.T) {
	// 0x12345678 在四种排列下的确定线路字序。
	words, _ := EncodeValue(TypeUint32, OrderABCD, 0x12345678)
	if words[0] != 0x1234 || words[1] != 0x5678 {
		t.Fatalf("ABCD=%x", words)
	}
	words, _ = EncodeValue(TypeUint32, OrderCDAB, 0x12345678)
	if words[0] != 0x5678 || words[1] != 0x1234 {
		t.Fatalf("CDAB=%x", words)
	}
	words, _ = EncodeValue(TypeUint32, OrderBADC, 0x12345678)
	if words[0] != 0x3412 || words[1] != 0x7856 {
		t.Fatalf("BADC=%x", words)
	}
	words, _ = EncodeValue(TypeUint32, OrderDCBA, 0x12345678)
	if words[0] != 0x7856 || words[1] != 0x3412 {
		t.Fatalf("DCBA=%x", words)
	}
}

func TestSplitChunksDeterministic(t *testing.T) {
	pts := []Point{
		{Name: "a", Region: RegionHoldingRegisters, Address: 10, Type: TypeUint16},
		{Name: "b", Region: RegionHoldingRegisters, Address: 12, Type: TypeFloat32, Order: OrderABCD},
		{Name: "c", Region: RegionInputRegisters, Address: 10, Type: TypeUint16},
		{Name: "d", Region: RegionCoils, Address: 3},
		{Name: "e", Region: RegionHoldingRegisters, Address: 11, Type: TypeUint16},
	}
	chunks, err := SplitChunks(pts)
	if err != nil {
		t.Fatal(err)
	}
	// 期望：coils[3..4), holding[10..14), input[10..11) —— 区域分组 + 地址排序合并。
	want := []Chunk{
		{RegionCoils, 3, 1},
		{RegionHoldingRegisters, 10, 4},
		{RegionInputRegisters, 10, 1},
	}
	if len(chunks) != len(want) {
		t.Fatalf("chunks=%+v", chunks)
	}
	for i := range want {
		if chunks[i] != want[i] {
			t.Fatalf("chunk %d: got %+v want %+v", i, chunks[i], want[i])
		}
	}
	t.Logf("chunks=%+v verdict=ok", chunks)
}

func TestSplitChunksCapacity(t *testing.T) {
	// 300 个连续寄存器点位 -> 125+125+50 三片。
	var pts []Point
	for i := 0; i < 300; i++ {
		pts = append(pts, Point{Name: string(rune('a'+i%26)) + string(rune('A'+i/26)),
			Region: RegionHoldingRegisters, Address: uint16(i), Type: TypeUint16})
	}
	chunks, err := SplitChunks(pts)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 || chunks[0].Quantity != 125 || chunks[1].Quantity != 125 || chunks[2].Quantity != 50 {
		t.Fatalf("chunks=%+v", chunks)
	}
}

func TestBatchReadPartialFailure(t *testing.T) {
	mem := NewSlaveMemory(64)
	mem.SetRegister(0, 0x0042)
	mem.SetRegister(2, 0x3F80) // float32 1.0 高位
	mem.SetRegister(3, 0x0000)
	mem.SetCoil(1, true)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	sl, _ := StartSlave(ln, SlaveConfig{Transport: TransportTCP, Memory: mem, UnitID: 1})
	defer sl.Close()
	sess, _ := NewSession(SessionConfig{
		Transport: TransportTCP, UnitID: 1, Dial: dialerFor(sl.Addr().String()),
		Timeout: time.Second, MaxRetry: 1,
	})
	defer sess.Close()
	c := NewClient(sess)

	points := []Point{
		{Name: "temp", Region: RegionHoldingRegisters, Address: 0, Type: TypeUint16},
		{Name: "flow", Region: RegionHoldingRegisters, Address: 2, Type: TypeFloat32, Order: OrderABCD},
		{Name: "valve", Region: RegionCoils, Address: 1},
		{Name: "bad", Region: RegionHoldingRegisters, Address: 9000, Type: TypeUint16}, // 越界
	}
	res, err := BatchRead(context.Background(), c, points)
	me := AsError(err)
	if me == nil || me.Kind != KindPartialFailure {
		t.Fatalf("want partial failure, got %v", err)
	}
	if res.Items["temp"].Value != uint16(0x42) {
		t.Fatalf("temp=%v", res.Items["temp"])
	}
	if f, ok := res.Items["flow"].Value.(float32); !ok || math.Abs(float64(f-1.0)) > 1e-6 {
		t.Fatalf("flow=%v", res.Items["flow"])
	}
	if res.Items["valve"].Value != true {
		t.Fatalf("valve=%v", res.Items["valve"])
	}
	bad := AsError(res.Items["bad"].Err)
	if bad == nil || bad.Kind != KindModbusException {
		t.Fatalf("bad point err=%v", res.Items["bad"].Err)
	}
	if res.FailedCount() != 1 {
		t.Fatalf("failed=%d", res.FailedCount())
	}
	t.Logf("verdict=%s temp=%v flow=%v valve=%v bad=%s",
		me.Kind, res.Items["temp"].Value, res.Items["flow"].Value,
		res.Items["valve"].Value, bad.Kind)
}
