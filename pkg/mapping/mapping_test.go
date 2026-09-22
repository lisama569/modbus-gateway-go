package mapping

import (
	"errors"
	"math"
	"testing"
)

func TestByteWordOrderDecode(t *testing.T) {
	// 32 位值 0x12345678,寄存器布局 [0x1234, 0x5678] 是大端 ABCD。
	regs := []uint16{0x1234, 0x5678}
	cases := []struct {
		order ByteOrder
		want  uint32
	}{
		{BigEndian, 0x12345678},            // ABCD
		{LittleEndian, 0x78563412},         // DCBA
		{BigEndianByteSwap, 0x56781234},    // CDAB
		{LittleEndianByteSwap, 0x34127856}, // BADC
	}
	for _, c := range cases {
		v, err := DecodeValue(TypeUint32, c.order, regs)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("order=%d regs=%04x,%04x => uint32=%08X", c.order, regs[0], regs[1], v)
		if v.(uint32) != c.want {
			t.Fatalf("order %d: want %08X, got %08X", c.order, c.want, v.(uint32))
		}
	}

	// float32 大端 ABCD
	fregs := []uint16{0x4049, 0x0FDB} // 3.1415927
	fv, err := DecodeValue(TypeFloat32, BigEndian, fregs)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(float64(fv.(float32))-3.1415927) > 1e-6 {
		t.Fatalf("float32 = %v", fv)
	}

	// 16 位类型不应用字序,固定大端
	u16, _ := DecodeValue(TypeUint16, LittleEndian, []uint16{0xABCD})
	if u16.(uint16) != 0xABCD {
		t.Fatalf("uint16 = %04X", u16)
	}

	// int64 / float64 宽度 4 寄存器
	i64, err := DecodeValue(TypeInt64, BigEndian, []uint16{0, 0, 0, 1})
	if err != nil || i64.(int64) != 1 {
		t.Fatalf("int64 = %v err=%v", i64, err)
	}

	// 宽度不符
	if _, err := DecodeValue(TypeUint32, BigEndian, []uint16{1}); err == nil {
		t.Fatal("expected width error")
	}
}

func TestPlanSegmentsSplitRules(t *testing.T) {
	points := []Point{
		{Name: "h0", Area: AreaHoldingRegisters, Address: 0, Type: TypeUint16},
		{Name: "h124", Area: AreaHoldingRegisters, Address: 124, Type: TypeUint16},
		{Name: "h200", Area: AreaHoldingRegisters, Address: 200, Type: TypeUint32, Order: BigEndian},
		{Name: "c0", Area: AreaCoils, Address: 0, Type: TypeBool},
		{Name: "c2100", Area: AreaCoils, Address: 2100, Type: TypeBool},
		{Name: "i0", Area: AreaInputRegisters, Address: 0, Type: TypeUint32, Order: BigEndian},
	}
	segs, err := PlanSegments(points)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range segs {
		t.Logf("segment[%d] area=%s addr=%d qty=%d points=%v",
			i, areaName(s.Area), s.Address, s.Quantity, s.Points)
	}
	// 期望:
	// coils: 2 段 ([0..0], [2100..2100],跨度 2101 > 2000)
	// holding: [0..124] 跨度 125 一段; [200..201] 第二段
	// input: [0..1] 一段
	want := []struct {
		area Area
		addr uint16
		qty  uint16
	}{
		{AreaCoils, 0, 1},
		{AreaCoils, 2100, 1},
		{AreaHoldingRegisters, 0, 125},
		{AreaHoldingRegisters, 200, 2},
		{AreaInputRegisters, 0, 2},
	}
	if len(segs) != len(want) {
		t.Fatalf("got %d segments, want %d", len(segs), len(want))
	}
	for i, w := range want {
		if segs[i].Area != w.area || segs[i].Address != w.addr || segs[i].Quantity != w.qty {
			t.Fatalf("segment[%d] = area=%d addr=%d qty=%d, want area=%d addr=%d qty=%d",
				i, segs[i].Area, segs[i].Address, segs[i].Quantity, w.area, w.addr, w.qty)
		}
	}

	// 输入顺序不影响拆分结果(确定性)
	shuffled := []Point{points[3], points[0], points[5], points[2], points[1], points[4]}
	segs2, _ := PlanSegments(shuffled)
	if len(segs2) != len(segs) {
		t.Fatalf("nondeterministic plan: %d vs %d", len(segs2), len(segs))
	}
	for i := range segs {
		a, b := segs[i], segs2[i]
		if a.Area != b.Area || a.Address != b.Address || a.Quantity != b.Quantity {
			t.Fatalf("segment %d differs under shuffled input", i)
		}
	}
}

func TestPlanSegmentsOverflow(t *testing.T) {
	// 宽点位不应被拆跨:地址 124 的 uint32 占 124/125,若并入 [0..] 跨度为 126>125,
	// 必须整体进入新帧。
	points := []Point{
		{Name: "a", Area: AreaHoldingRegisters, Address: 0, Type: TypeUint16},
		{Name: "wide", Area: AreaHoldingRegisters, Address: 124, Type: TypeUint32, Order: BigEndian},
	}
	segs, err := PlanSegments(points)
	if err != nil {
		t.Fatal(err)
	}
	// [0..0] 与 [123..124] 是两段
	if len(segs) != 2 {
		t.Fatalf("wide point must not be split across frames, got %d segments", len(segs))
	}
	t.Logf("wide point split: %+v", segs)
}

// fakeReader 是可控的底层读取器,用于注入片段级失败。
type fakeReader struct {
	holding map[uint16]uint16
	failAt  map[uint16]bool // 起始地址命中即失败
}

func (f *fakeReader) ReadCoils(addr, qty uint16) ([]bool, error) {
	return make([]bool, qty), nil
}
func (f *fakeReader) ReadDiscreteInputs(addr, qty uint16) ([]bool, error) {
	return make([]bool, qty), nil
}
func (f *fakeReader) ReadHoldingRegisters(addr, qty uint16) ([]uint16, error) {
	if f.failAt[addr] {
		return nil, errors.New("simulated device error at segment " + areaName(AreaHoldingRegisters))
	}
	out := make([]uint16, qty)
	for i := range out {
		out[i] = f.holding[addr+uint16(i)]
	}
	return out, nil
}
func (f *fakeReader) ReadInputRegisters(addr, qty uint16) ([]uint16, error) {
	return make([]uint16, qty), nil
}

func TestReadBatchPartialFailure(t *testing.T) {
	points := []Point{
		{Name: "good", Area: AreaHoldingRegisters, Address: 0, Type: TypeUint32, Order: BigEndian},
		{Name: "bad", Area: AreaHoldingRegisters, Address: 200, Type: TypeUint16},
	}
	fr := &fakeReader{
		holding: map[uint16]uint16{0: 0x1234, 1: 0x5678},
		failAt:  map[uint16]bool{200: true},
	}
	res, err := ReadBatch(fr, points)
	if err == nil {
		t.Fatal("expected partial error")
	}
	if res.OK {
		t.Fatal("batch should not be OK")
	}
	if res.Succeeded != 1 || res.Failed != 1 {
		t.Fatalf("want 1 success 1 fail, got %d/%d", res.Succeeded, res.Failed)
	}
	if res.Results[0].Err != nil || res.Results[0].Value.(uint32) != 0x12345678 {
		t.Fatalf("good point corrupted: %+v", res.Results[0])
	}
	if res.Results[1].Err == nil {
		t.Fatal("bad point must carry error")
	}
	t.Logf("partial result: good=%v bad.err=%v overall=%s",
		res.Results[0].Value, res.Results[1].Err, err)
}

func TestReadBatchAllSuccess(t *testing.T) {
	points := []Point{
		{Name: "a", Area: AreaHoldingRegisters, Address: 10, Type: TypeFloat32, Order: BigEndian},
	}
	fr := &fakeReader{holding: map[uint16]uint16{10: 0x4049, 11: 0x0FDB}}
	res, err := ReadBatch(fr, points)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatal("should be OK")
	}
	fv := res.Results[0].Value.(float32)
	if math.Abs(float64(fv)-3.1415927) > 1e-6 {
		t.Fatalf("fv=%v", fv)
	}
}

func TestReadBatchCrossArea(t *testing.T) {
	points := []Point{
		{Name: "h", Area: AreaHoldingRegisters, Address: 0, Type: TypeUint16},
		{Name: "i", Area: AreaInputRegisters, Address: 0, Type: TypeUint16},
		{Name: "c", Area: AreaCoils, Address: 0, Type: TypeBool},
		{Name: "d", Area: AreaDiscreteInputs, Address: 0, Type: TypeBool},
	}
	fr := &fakeReader{holding: map[uint16]uint16{0: 7}}
	res, err := ReadBatch(fr, points)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Segments) != 4 {
		t.Fatalf("cross-area must use 4 segments, got %d", len(res.Segments))
	}
	if res.Results[0].Value.(uint16) != 7 {
		t.Fatal("holding value mismatch")
	}
}
