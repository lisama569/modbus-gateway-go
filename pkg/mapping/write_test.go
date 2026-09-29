package mapping

import (
	"errors"
	"testing"
)

// fakeWriter 记录写调用并可按起始地址注入失败。
type fakeWriter struct {
	mu        chan struct{}
	coilSegs  []fakeCoilSeg
	regSegs   []fakeRegSeg
	failCoil  map[uint16]bool
	failReg   map[uint16]bool
	uncertain map[uint16]bool // 起始地址命中返回"不确定"错误
}

type fakeCoilSeg struct {
	addr uint16
	bits []bool
}
type fakeRegSeg struct {
	addr uint16
	regs []uint16
}

func newFakeWriter() *fakeWriter {
	return &fakeWriter{
		failCoil: map[uint16]bool{}, failReg: map[uint16]bool{},
		uncertain: map[uint16]bool{},
	}
}

func (f *fakeWriter) WriteCoils(addr uint16, bits []bool) (int, error) {
	f.coilSegs = append(f.coilSegs, fakeCoilSeg{addr, append([]bool(nil), bits...)})
	if f.failCoil[addr] {
		return 1, errors.New("simulated coil write reject")
	}
	return 1, nil
}

func (f *fakeWriter) WriteRegisters(addr uint16, regs []uint16) (int, error) {
	f.regSegs = append(f.regSegs, fakeRegSeg{addr, append([]uint16(nil), regs...)})
	if f.uncertain[addr] {
		return 3, fakeUncertainErr("may have applied")
	}
	if f.failReg[addr] {
		return 1, errors.New("simulated register write reject")
	}
	return 1, nil
}

type fakeUncertainErr string

func (e fakeUncertainErr) Error() string          { return string(e) }
func (e fakeUncertainErr) UncertainOutcome() bool { return true }

func hp(name string, addr uint16, t DataType, o ByteOrder) Point {
	return Point{Name: name, Area: AreaHoldingRegisters, Address: addr, Type: t, Order: o}
}

func TestWriteBatchReadOnlyAndTypeMismatch(t *testing.T) {
	items := []WriteItem{
		{Point: Point{Name: "di", Area: AreaDiscreteInputs, Address: 0, Type: TypeBool}, Value: true},
		{Point: Point{Name: "ir", Area: AreaInputRegisters, Address: 0, Type: TypeUint16}, Value: uint16(1)},
		{Point: Point{Name: "coil-bad", Area: AreaCoils, Address: 0, Type: TypeBool}, Value: 1},
		{Point: hp("reg-bad", 0, TypeUint16, BigEndian), Value: true},
		{Point: hp("reg-badw", 2, TypeUint32, BigEndian), Value: uint16(1)},
	}
	fw := newFakeWriter()
	res, err := WriteBatch(fw, items)
	if err == nil {
		t.Fatal("expected partial error")
	}
	if res.OK || res.Succeeded != 0 || res.Failed != 5 {
		t.Fatalf("want all 5 failed, got ok=%v s=%d f=%d", res.OK, res.Succeeded, res.Failed)
	}
	kinds := map[int]string{}
	for i, r := range res.Results {
		me := r.Err.(*MapError)
		kinds[i] = me.Kind
		if me.Kind != "plan" && me.Kind != "value" {
			t.Fatalf("item %d unexpected kind %s", i, me.Kind)
		}
	}
	if kinds[0] != "plan" || kinds[1] != "plan" {
		t.Fatalf("read-only must be plan failures: %v", kinds)
	}
	if kinds[2] != "value" || kinds[3] != "value" || kinds[4] != "value" {
		t.Fatalf("type mismatch must be value failures: %v", kinds)
	}
	if len(fw.coilSegs) != 0 || len(fw.regSegs) != 0 {
		t.Fatal("invalid items must never reach transport")
	}
}

func TestWriteBatchCoilAndRegisterPayload(t *testing.T) {
	items := []WriteItem{
		{Point: Point{Name: "c1", Area: AreaCoils, Address: 3, Type: TypeBool}, Value: true},
		{Point: Point{Name: "c0", Area: AreaCoils, Address: 0, Type: TypeBool}, Value: true},
		{Point: Point{Name: "c2", Area: AreaCoils, Address: 4, Type: TypeBool}, Value: false},
		{Point: hp("w", 10, TypeUint32, LittleEndian), Value: uint32(0x12345678)},
	}
	fw := newFakeWriter()
	res, err := WriteBatch(fw, items)
	if err != nil {
		t.Fatal(err)
	}
	// 线圈 0 与 3/4 不连续 -> 两段
	if len(fw.coilSegs) != 2 {
		t.Fatalf("want 2 coil segments, got %d: %+v", len(fw.coilSegs), fw.coilSegs)
	}
	if fw.coilSegs[0].addr != 0 || len(fw.coilSegs[0].bits) != 1 || !fw.coilSegs[0].bits[0] {
		t.Fatalf("seg0 = %+v", fw.coilSegs[0])
	}
	if fw.coilSegs[1].addr != 3 || len(fw.coilSegs[1].bits) != 2 ||
		!fw.coilSegs[1].bits[0] || fw.coilSegs[1].bits[1] {
		t.Fatalf("seg1 = %+v", fw.coilSegs[1])
	}
	// 寄存器:DCBA 布局
	if len(fw.regSegs) != 1 || fw.regSegs[0].addr != 10 ||
		fw.regSegs[0].regs[0] != 0x7856 || fw.regSegs[0].regs[1] != 0x3412 {
		t.Fatalf("reg seg = %+v", fw.regSegs)
	}
	for i, r := range res.Results {
		if r.Err != nil || !r.Confirmed {
			t.Fatalf("result[%d] not confirmed: %+v", i, r)
		}
	}
}

func TestWriteBatchGapNeverCovered(t *testing.T) {
	// 地址 0 与 5 之间有间隙,绝不允许一段 FC0F/FC10 顺带改写间隙。
	items := []WriteItem{
		{Point: hp("a", 0, TypeUint16, BigEndian), Value: uint16(1)},
		{Point: hp("b", 5, TypeUint16, BigEndian), Value: uint16(2)},
	}
	fw := newFakeWriter()
	if _, err := WriteBatch(fw, items); err != nil {
		t.Fatal(err)
	}
	if len(fw.regSegs) != 2 {
		t.Fatalf("gap must split writes, got %d segments", len(fw.regSegs))
	}
	for _, s := range fw.regSegs {
		if len(s.regs) != 1 {
			t.Fatalf("segment must cover exactly target cells: %+v", s)
		}
	}
}

func TestWriteBatchCapacitySplit(t *testing.T) {
	// 连续 124 个保持寄存器:单帧 123,必须拆 123 + 1。
	items := make([]WriteItem, 124)
	for i := range items {
		items[i] = WriteItem{Point: hp("p", uint16(i), TypeUint16, BigEndian), Value: uint16(i + 1)}
	}
	fw := newFakeWriter()
	res, err := WriteBatch(fw, items)
	if err != nil {
		t.Fatal(err)
	}
	if len(fw.regSegs) != 2 {
		t.Fatalf("want 2 segments, got %d", len(fw.regSegs))
	}
	if fw.regSegs[0].addr != 0 || len(fw.regSegs[0].regs) != 123 {
		t.Fatalf("seg0 = addr=%d len=%d", fw.regSegs[0].addr, len(fw.regSegs[0].regs))
	}
	if fw.regSegs[1].addr != 123 || len(fw.regSegs[1].regs) != 1 {
		t.Fatalf("seg1 = addr=%d len=%d", fw.regSegs[1].addr, len(fw.regSegs[1].regs))
	}
	// 结果与输入同序
	for i := range items {
		if res.Results[i].Index != i || res.Results[i].Err != nil {
			t.Fatalf("result order/value mismatch at %d: %+v", i, res.Results[i])
		}
	}

	// 宽点位不允许跨帧:地址 122 的 uint32 需独立成段。
	wide := []WriteItem{
		{Point: hp("a", 0, TypeUint16, BigEndian), Value: uint16(1)},
		{Point: hp("w", 122, TypeUint32, BigEndian), Value: uint32(9)},
	}
	fw2 := newFakeWriter()
	if _, err := WriteBatch(fw2, wide); err != nil {
		t.Fatal(err)
	}
	if len(fw2.regSegs) != 2 || fw2.regSegs[1].addr != 122 || len(fw2.regSegs[1].regs) != 2 {
		t.Fatalf("wide point must start its own frame: %+v", fw2.regSegs)
	}
}

func TestWriteBatchCoilCapacitySplit(t *testing.T) {
	items := make([]WriteItem, 1969)
	for i := range items {
		items[i] = WriteItem{
			Point: Point{Name: "c", Area: AreaCoils, Address: uint16(i), Type: TypeBool},
			Value: i%2 == 0,
		}
	}
	fw := newFakeWriter()
	if _, err := WriteBatch(fw, items); err != nil {
		t.Fatal(err)
	}
	if len(fw.coilSegs) != 2 || len(fw.coilSegs[0].bits) != 1968 ||
		fw.coilSegs[1].addr != 1968 || len(fw.coilSegs[1].bits) != 1 {
		t.Fatalf("coil split wrong: %+v", fw.coilSegs)
	}
}

func TestWriteBatchCrossAreaNotMerged(t *testing.T) {
	items := []WriteItem{
		{Point: Point{Name: "c", Area: AreaCoils, Address: 0, Type: TypeBool}, Value: true},
		{Point: hp("h", 0, TypeUint16, BigEndian), Value: uint16(7)},
	}
	fw := newFakeWriter()
	res, err := WriteBatch(fw, items)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Segments) != 2 {
		t.Fatalf("cross-area must not merge: %d", len(res.Segments))
	}
}

func TestWriteBatchOverlapRejected(t *testing.T) {
	items := []WriteItem{
		{Point: hp("wide", 10, TypeUint32, BigEndian), Value: uint32(0x11223344)},
		{Point: hp("inside", 11, TypeUint16, BigEndian), Value: uint16(5)},
	}
	fw := newFakeWriter()
	res, _ := WriteBatch(fw, items)
	if res.Failed != 2 {
		t.Fatalf("both overlapping points must fail, got %d", res.Failed)
	}
	for _, r := range res.Results {
		if r.Err.(*MapError).Kind != "plan" {
			t.Fatalf("overlap must be plan failure: %v", r.Err)
		}
	}
	if len(fw.regSegs) != 0 {
		t.Fatal("overlapping points must not be written")
	}
}

func TestWriteBatchPartialFailurePreservesSuccess(t *testing.T) {
	items := []WriteItem{
		{Point: hp("ok", 0, TypeUint16, BigEndian), Value: uint16(1)},
		{Point: hp("reject", 100, TypeUint16, BigEndian), Value: uint16(2)},
	}
	fw := newFakeWriter()
	fw.failReg[100] = true
	res, err := WriteBatch(fw, items)
	if err == nil {
		t.Fatal("expected partial")
	}
	if res.OK || res.Succeeded != 1 || res.Failed != 1 {
		t.Fatalf("s=%d f=%d", res.Succeeded, res.Failed)
	}
	if res.Results[0].Err != nil || !res.Results[0].Confirmed {
		t.Fatal("succeeded point must remain visible and confirmed")
	}
	if res.Results[1].Err == nil || res.Results[1].Err.(*MapError).Kind != "transport" {
		t.Fatalf("rejected point: %+v", res.Results[1])
	}
}

func TestWriteBatchUncertainKind(t *testing.T) {
	items := []WriteItem{
		{Point: hp("u", 0, TypeUint16, BigEndian), Value: uint16(1)},
	}
	fw := newFakeWriter()
	fw.uncertain[0] = true
	res, _ := WriteBatch(fw, items)
	if res.Results[0].Err.(*MapError).Kind != "uncertain" {
		t.Fatalf("want uncertain, got %v", res.Results[0].Err)
	}
	if res.Results[0].Attempts != 3 {
		t.Fatalf("attempts must propagate, got %d", res.Results[0].Attempts)
	}
}

func TestWriteBatchDeterministicOrder(t *testing.T) {
	items := []WriteItem{
		{Point: hp("b", 5, TypeUint16, BigEndian), Value: uint16(2)},
		{Point: hp("a", 0, TypeUint16, BigEndian), Value: uint16(1)},
		{Point: Point{Name: "c", Area: AreaCoils, Address: 0, Type: TypeBool}, Value: true},
	}
	fw1 := newFakeWriter()
	r1, _ := WriteBatch(fw1, items)
	fw2 := newFakeWriter()
	r2, _ := WriteBatch(fw2, items)
	if len(r1.Segments) != len(r2.Segments) {
		t.Fatal("nondeterministic segment count")
	}
	for i := range r1.Segments {
		a, b := r1.Segments[i], r2.Segments[i]
		if a.Area != b.Area || a.Address != b.Address || a.Quantity != b.Quantity {
			t.Fatalf("segment %d differs", i)
		}
	}
}
