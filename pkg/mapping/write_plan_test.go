package mapping

import (
	"testing"
)

func TestPlanWriteSegmentsRules(t *testing.T) {
	reqs := []WriteRequest{
		{Point: Point{Name: "c0", Area: AreaCoils, Address: 0, Type: TypeBool}, Value: true},
		{Point: Point{Name: "c1", Area: AreaCoils, Address: 1, Type: TypeBool}, Value: false},
		{Point: Point{Name: "c3", Area: AreaCoils, Address: 3, Type: TypeBool}, Value: true}, // 与 c1 有间隙 2
		{Point: Point{Name: "h0", Area: AreaHoldingRegisters, Address: 0, Type: TypeUint32, Order: BigEndian}, Value: uint32(1)},
		{Point: Point{Name: "h2", Area: AreaHoldingRegisters, Address: 2, Type: TypeUint16}, Value: uint16(2)},
		{Point: Point{Name: "h4", Area: AreaHoldingRegisters, Address: 4, Type: TypeUint16}, Value: uint16(4)}, // 连续
	}
	segs, errs := PlanWriteSegments(reqs)
	for i, e := range errs {
		if e != nil {
			t.Fatalf("req[%d] unexpected error: %v", i, e)
		}
	}
	for i, s := range segs {
		t.Logf("segment[%d] area=%s addr=%d qty=%d points=%v",
			i, areaName(s.Area), s.Address, s.Quantity, s.Points)
	}
	// 期望:
	// coils [0..1] 连续一段;地址 3 因间隙 2 不并入,独立一段
	// holding: [0..1] 宽点 + 地址2 + 地址4?地址2结尾3,地址4起点有间隙3 -> 拆:
	//   [0..2] (h0 宽2 + h2 连续),[4..4] 独立
	want := []struct {
		area Area
		addr uint16
		qty  uint16
	}{
		{AreaCoils, 0, 2},
		{AreaCoils, 3, 1},
		{AreaHoldingRegisters, 0, 3},
		{AreaHoldingRegisters, 4, 1},
	}
	if len(segs) != len(want) {
		t.Fatalf("got %d segments, want %d", len(segs), len(want))
	}
	for i, w := range want {
		if segs[i].Area != w.area || segs[i].Address != w.addr || segs[i].Quantity != w.qty {
			t.Fatalf("segment[%d]=area=%d addr=%d qty=%d want area=%d addr=%d qty=%d",
				i, segs[i].Area, segs[i].Address, segs[i].Quantity, w.area, w.addr, w.qty)
		}
	}

	// 确定性:打乱输入,片段集合不变
	shuffled := []WriteRequest{reqs[5], reqs[0], reqs[3], reqs[2], reqs[1], reqs[4]}
	segs2, _ := PlanWriteSegments(shuffled)
	if len(segs2) != len(segs) {
		t.Fatalf("nondeterministic: %d vs %d", len(segs2), len(segs))
	}
	for i := range segs {
		if segs[i].Area != segs2[i].Area || segs[i].Address != segs2[i].Address ||
			segs[i].Quantity != segs2[i].Quantity {
			t.Fatalf("segment %d differs under shuffle", i)
		}
	}
}

func TestPlanWriteCapacitySplit(t *testing.T) {
	// 124 个连续 uint16:必须拆成 123 + 1
	reqs := make([]WriteRequest, 124)
	for i := range reqs {
		reqs[i] = WriteRequest{
			Point: Point{Name: "r", Area: AreaHoldingRegisters,
				Address: uint16(i), Type: TypeUint16},
			Value: uint16(i),
		}
	}
	segs, errs := PlanWriteSegments(reqs)
	for i, e := range errs {
		if e != nil {
			t.Fatalf("req[%d]: %v", i, e)
		}
	}
	if len(segs) != 2 || segs[0].Quantity != 123 || segs[1].Quantity != 1 ||
		segs[1].Address != 123 {
		t.Fatalf("register capacity split wrong: %+v", segs)
	}

	// 宽点不跨帧:122 个连续 uint16 + 地址122 的 uint32(宽2),
	// 并入会超出 123,宽点整体进新帧
	reqs2 := make([]WriteRequest, 0, 123)
	for i := 0; i < 122; i++ {
		reqs2 = append(reqs2, WriteRequest{
			Point: Point{Name: "r", Area: AreaHoldingRegisters,
				Address: uint16(i), Type: TypeUint16},
			Value: uint16(i),
		})
	}
	reqs2 = append(reqs2, WriteRequest{
		Point: Point{Name: "wide", Area: AreaHoldingRegisters,
			Address: 122, Type: TypeUint32, Order: BigEndian},
		Value: uint32(9),
	})
	segs2, errs2 := PlanWriteSegments(reqs2)
	for i, e := range errs2 {
		if e != nil {
			t.Fatalf("req[%d]: %v", i, e)
		}
	}
	if len(segs2) != 2 || segs2[1].Address != 122 || segs2[1].Quantity != 2 {
		t.Fatalf("wide point must move whole to new segment: %+v", segs2)
	}

	// 线圈容量 1968:1969 个连续线圈拆 1968+1
	creqs := make([]WriteRequest, 1969)
	for i := range creqs {
		creqs[i] = WriteRequest{
			Point: Point{Name: "c", Area: AreaCoils,
				Address: uint16(i), Type: TypeBool},
			Value: true,
		}
	}
	csegs, cerrs := PlanWriteSegments(creqs)
	for i, e := range cerrs {
		if e != nil {
			t.Fatalf("creq[%d]: %v", i, e)
		}
	}
	if len(csegs) != 2 || csegs[0].Quantity != 1968 || csegs[1].Quantity != 1 {
		t.Fatalf("coil capacity split wrong: %+v", csegs)
	}
}

func TestPlanWriteRejectsReadOnlyAndMismatch(t *testing.T) {
	reqs := []WriteRequest{
		{Point: Point{Name: "di", Area: AreaDiscreteInputs, Address: 0, Type: TypeBool}, Value: true},
		{Point: Point{Name: "ir", Area: AreaInputRegisters, Address: 0, Type: TypeUint16}, Value: uint16(1)},
		{Point: Point{Name: "coilbad", Area: AreaCoils, Address: 1, Type: TypeBool}, Value: 1},
		{Point: Point{Name: "regtype", Area: AreaHoldingRegisters, Address: 0, Type: TypeUint32, Order: BigEndian}, Value: uint16(1)},
		{Point: Point{Name: "regbool", Area: AreaHoldingRegisters, Address: 2, Type: TypeBool}, Value: true},
		{Point: Point{Name: "ok", Area: AreaHoldingRegisters, Address: 4, Type: TypeUint16}, Value: uint16(1)},
	}
	segs, errs := PlanWriteSegments(reqs)
	for i := 0; i < 5; i++ {
		if errs[i] == nil {
			t.Fatalf("req[%d] must fail", i)
		}
		t.Logf("req[%d] rejected: %v", i, errs[i])
	}
	if errs[5] != nil {
		t.Fatalf("valid point rejected: %v", errs[5])
	}
	// 只有一个合法点 -> 只有一段,失败点绝不进入任何片段
	if len(segs) != 1 || len(segs[0].Points) != 1 || segs[0].Points[0] != 5 {
		t.Fatalf("invalid points must not appear in segments: %+v", segs)
	}
}

func TestPlanWriteOverlapRejectsBoth(t *testing.T) {
	reqs := []WriteRequest{
		{Point: Point{Name: "a", Area: AreaHoldingRegisters, Address: 0, Type: TypeUint32, Order: BigEndian}, Value: uint32(1)},
		{Point: Point{Name: "b", Area: AreaHoldingRegisters, Address: 1, Type: TypeUint16}, Value: uint16(2)}, // 与 a 重叠
		{Point: Point{Name: "c", Area: AreaHoldingRegisters, Address: 4, Type: TypeUint16}, Value: uint16(3)},
	}
	segs, errs := PlanWriteSegments(reqs)
	if errs[0] == nil || errs[1] == nil {
		t.Fatalf("overlapping points must both fail: %v %v", errs[0], errs[1])
	}
	if errs[2] != nil {
		t.Fatalf("non-overlapping point rejected: %v", errs[2])
	}
	if len(segs) != 1 || segs[0].Points[0] != 2 {
		t.Fatalf("only non-overlapping point should be planned: %+v", segs)
	}
}
