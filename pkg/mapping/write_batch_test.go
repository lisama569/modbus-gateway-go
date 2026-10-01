package mapping

import (
	"sync"
	"testing"

	"github.com/lisama569/modbus-gateway-go/pkg/modbus"
)

// fakeWriteTransport 是带内存四区的可控写传输。
// writeHook 决定某次下发的结果:
//   - err 非 nil 表示该次下发没有拿到成功应答;
//   - applied 表示设备内存是否实际被改写(模拟"响应丢失但写入已落地")。
//
// 读方法始终读取当前内存,因此读回确认反映真实最终状态。
type fakeWriteTransport struct {
	mu        sync.Mutex
	coils     map[uint16]bool
	holding   map[uint16]uint16
	writeHook func(addr uint16) (err error, applied bool)
	calls     map[uint16]int
}

func newFakeWriteTransport() *fakeWriteTransport {
	return &fakeWriteTransport{
		coils:   map[uint16]bool{},
		holding: map[uint16]uint16{},
		calls:   map[uint16]int{},
	}
}

func (f *fakeWriteTransport) ReadCoils(addr, qty uint16) ([]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]bool, qty)
	for i := range out {
		out[i] = f.coils[addr+uint16(i)]
	}
	return out, nil
}
func (f *fakeWriteTransport) ReadDiscreteInputs(addr, qty uint16) ([]bool, error) {
	return make([]bool, qty), nil
}
func (f *fakeWriteTransport) ReadHoldingRegisters(addr, qty uint16) ([]uint16, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uint16, qty)
	for i := range out {
		out[i] = f.holding[addr+uint16(i)]
	}
	return out, nil
}
func (f *fakeWriteTransport) ReadInputRegisters(addr, qty uint16) ([]uint16, error) {
	return make([]uint16, qty), nil
}

// doWrite 记录次数、执行钩子,并按"是否实际生效"改写内存,返回应答错误。
func (f *fakeWriteTransport) doWrite(addr uint16, apply func()) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[addr]++
	var err error
	applied := true
	if f.writeHook != nil {
		err, applied = f.writeHook(addr)
	}
	if err == nil || applied {
		apply()
	}
	return err
}

func (f *fakeWriteTransport) WriteSingleCoil(addr uint16, on bool) error {
	return f.doWrite(addr, func() { f.coils[addr] = on })
}
func (f *fakeWriteTransport) WriteSingleRegister(addr, value uint16) error {
	return f.doWrite(addr, func() { f.holding[addr] = value })
}
func (f *fakeWriteTransport) WriteMultipleCoils(addr uint16, bits []bool) error {
	return f.doWrite(addr, func() {
		for i, b := range bits {
			f.coils[addr+uint16(i)] = b
		}
	})
}
func (f *fakeWriteTransport) WriteMultipleRegisters(addr uint16, regs []uint16) error {
	return f.doWrite(addr, func() {
		for i, v := range regs {
			f.holding[addr+uint16(i)] = v
		}
	})
}

func (f *fakeWriteTransport) callCount(addr uint16) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[addr]
}

func timeoutErr() error { return &modbus.Error{Kind: modbus.KindTimeout, Msg: "simulated timeout"} }
func rejectErr() error {
	return &modbus.Error{Kind: modbus.KindExceptionResponse,
		ExceptionCode: modbus.ExcIllegalDataValue, Msg: "simulated exception: illegal data value"}
}

func TestWriteBatchAllSuccess(t *testing.T) {
	ft := newFakeWriteTransport()
	reqs := []WriteRequest{
		{Point: Point{Name: "enable", Area: AreaCoils, Address: 0, Type: TypeBool}, Value: true},
		{Point: Point{Name: "setpoint", Area: AreaHoldingRegisters, Address: 10,
			Type: TypeFloat32, Order: BigEndian}, Value: float32(36.6)},
		{Point: Point{Name: "count", Area: AreaHoldingRegisters, Address: 12,
			Type: TypeUint32, Order: LittleEndian}, Value: uint32(1234)},
		{Point: Point{Name: "raw", Area: AreaHoldingRegisters, Address: 20, Type: TypeUint16}, Value: uint16(0xABCD)},
	}
	res, err := WriteBatch(ft, reqs, WriteConfig{})
	if err != nil || !res.OK || res.Succeeded != 4 {
		t.Fatalf("want all success, got ok=%v succ=%d fail=%d err=%v",
			res.OK, res.Succeeded, res.Failed, err)
	}
	for _, r := range res.Results {
		if r.Attempts != 1 || r.Confirmed {
			t.Fatalf("point %s attempts=%d confirmed=%v, want 1/false",
				r.Point.Name, r.Attempts, r.Confirmed)
		}
		if r.Status != StatusWritten || !r.Succeeded() {
			t.Fatalf("point %s want StatusWritten, got %s", r.Point.Name, r.Status)
		}
	}
	if v := ft.holding[20]; v != 0xABCD {
		t.Fatalf("uint16 not written: %04X", v)
	}
	// float32 36.6 大端 = 0x4212 6666
	if ft.holding[10] != 0x4212 || ft.holding[11] != 0x6666 {
		t.Fatalf("float32 wire wrong: %04X %04X", ft.holding[10], ft.holding[11])
	}
	// uint32 1234 DCBA = [0xD204, 0x0000]
	if ft.holding[12] != 0xD204 || ft.holding[13] != 0x0000 {
		t.Fatalf("uint32 LE wire wrong: %04X %04X", ft.holding[12], ft.holding[13])
	}
	if !ft.coils[0] {
		t.Fatal("coil not written")
	}
}

func TestWriteBatchPartialFailureKeepsSuccess(t *testing.T) {
	ft := newFakeWriteTransport()
	// 地址 0 的片段明确拒绝;地址 10 的片段正常。
	ft.writeHook = func(addr uint16) (error, bool) {
		if addr == 0 {
			return rejectErr(), false
		}
		return nil, true
	}
	reqs := []WriteRequest{
		{Point: Point{Name: "bad", Area: AreaCoils, Address: 0, Type: TypeBool}, Value: true},
		{Point: Point{Name: "good", Area: AreaHoldingRegisters, Address: 10, Type: TypeUint16}, Value: uint16(7)},
	}
	res, err := WriteBatch(ft, reqs, WriteConfig{})
	if err == nil || res.OK {
		t.Fatal("want partial failure")
	}
	if res.Succeeded != 1 || res.Failed != 1 {
		t.Fatalf("want 1/1, got %d/%d", res.Succeeded, res.Failed)
	}
	if res.Results[0].Err == nil {
		t.Fatal("bad point must carry error")
	}
	if res.Results[1].Err != nil || res.Results[1].Value.(uint16) != 7 {
		t.Fatalf("good point must be preserved: %+v", res.Results[1])
	}
	if ft.holding[10] != 7 {
		t.Fatal("successful write must land even when another segment failed")
	}
}

func TestWriteBatchDefiniteRejectNotRetried(t *testing.T) {
	ft := newFakeWriteTransport()
	ft.writeHook = func(addr uint16) (error, bool) {
		return rejectErr(), false // 每次(若被重试)都拒绝
	}
	reqs := []WriteRequest{
		{Point: Point{Name: "c", Area: AreaCoils, Address: 5, Type: TypeBool}, Value: true},
	}
	res, err := WriteBatch(ft, reqs, WriteConfig{MaxAttempts: 5})
	if err == nil || res.OK {
		t.Fatal("want failure")
	}
	if n := ft.callCount(5); n != 1 {
		t.Fatalf("definite reject must issue exactly once, got %d", n)
	}
	if res.Results[0].Attempts != 1 || res.Results[0].Confirmed {
		t.Fatalf("attempts=%d confirmed=%v", res.Results[0].Attempts, res.Results[0].Confirmed)
	}
	if res.Results[0].Status != StatusRejected || !res.Results[0].Rejected() {
		t.Fatalf("want StatusRejected, got %s", res.Results[0].Status)
	}
	if ft.coils[5] {
		t.Fatal("rejected write must not be applied")
	}
}

func TestWriteBatchUncertainButAppliedConfirmed(t *testing.T) {
	ft := newFakeWriteTransport()
	// 第一次下发实际已生效,但应答丢失(超时);读回应能确认。
	ft.writeHook = func(addr uint16) (error, bool) {
		return timeoutErr(), true
	}
	reqs := []WriteRequest{
		{Point: Point{Name: "v", Area: AreaHoldingRegisters, Address: 0, Type: TypeUint16}, Value: uint16(0x1234)},
	}
	res, err := WriteBatch(ft, reqs, WriteConfig{MaxAttempts: 3})
	if err != nil || !res.OK {
		t.Fatalf("write actually applied must be confirmed: %v", err)
	}
	if n := ft.callCount(0); n != 1 {
		t.Fatalf("confirmed write must not be re-issued, got %d", n)
	}
	if !res.Results[0].Confirmed || res.Results[0].Attempts != 1 {
		t.Fatalf("want confirmed via readback, got attempts=%d confirmed=%v",
			res.Results[0].Attempts, res.Results[0].Confirmed)
	}
	if res.Results[0].Status != StatusConfirmed {
		t.Fatalf("want StatusConfirmed, got %s", res.Results[0].Status)
	}
}

func TestWriteBatchUncertainNotAppliedRetriesThenSucceeds(t *testing.T) {
	ft := newFakeWriteTransport()
	// 前两次超时且未生效,第三次成功。
	count := 0
	ft.writeHook = func(addr uint16) (error, bool) {
		count++
		if count >= 3 {
			return nil, true
		}
		return timeoutErr(), false
	}
	reqs := []WriteRequest{
		{Point: Point{Name: "v", Area: AreaCoils, Address: 2, Type: TypeBool}, Value: true},
	}
	res, err := WriteBatch(ft, reqs, WriteConfig{MaxAttempts: 3})
	if err != nil || !res.OK {
		t.Fatalf("expected recovery: %v", err)
	}
	if count != 3 {
		t.Fatalf("want 3 physical attempts, got %d", count)
	}
	if res.Results[0].Attempts != 3 || res.Results[0].Confirmed {
		t.Fatalf("want attempts=3 confirmed=false(explicit echo), got %d/%v",
			res.Results[0].Attempts, res.Results[0].Confirmed)
	}
	if !ft.coils[2] {
		t.Fatal("value not applied")
	}
}

func TestWriteBatchUncertainExhaustedFails(t *testing.T) {
	ft := newFakeWriteTransport()
	ft.writeHook = func(addr uint16) (error, bool) {
		return timeoutErr(), false // 始终超时且不生效
	}
	reqs := []WriteRequest{
		{Point: Point{Name: "v", Area: AreaHoldingRegisters, Address: 0, Type: TypeUint16}, Value: uint16(99)},
	}
	res, err := WriteBatch(ft, reqs, WriteConfig{MaxAttempts: 3})
	if err == nil || res.OK {
		t.Fatal("uncertain-exhausted must be failure, not false success")
	}
	if res.Results[0].Err == nil || res.Results[0].Attempts != 3 {
		t.Fatalf("result wrong: %+v", res.Results[0])
	}
	if res.Results[0].Status != StatusUncertain || !res.Results[0].Uncertain() {
		t.Fatalf("exhausted uncertain must be StatusUncertain, got %s",
			res.Results[0].Status)
	}
	if ft.holding[0] != 0 {
		t.Fatal("unapplied write must not be visible")
	}
}

func TestWriteBatchConfirmOffRetriesWithoutReadback(t *testing.T) {
	ft := newFakeWriteTransport()
	// 第一次实际已生效但超时。关闭读回确认时不应识别为成功,而是继续重发,
	// 第二次拿到明确成功应答(不标记 Confirmed)。
	count := 0
	ft.writeHook = func(addr uint16) (error, bool) {
		count++
		if count == 1 {
			return timeoutErr(), true
		}
		return nil, true
	}
	reqs := []WriteRequest{
		{Point: Point{Name: "v", Area: AreaHoldingRegisters, Address: 0, Type: TypeUint16}, Value: uint16(5)},
	}
	res, err := WriteBatch(ft, reqs, WriteConfig{MaxAttempts: 3, Confirm: ConfirmOff})
	if err != nil || !res.OK {
		t.Fatalf("retry should succeed: %v", err)
	}
	if count != 2 {
		t.Fatalf("want 2 physical issues without readback short-circuit, got %d", count)
	}
	if res.Results[0].Confirmed {
		t.Fatal("explicit echo must not be marked confirmed")
	}
}

func TestWriteBatchStaticFailuresNoIssue(t *testing.T) {
	ft := newFakeWriteTransport()
	issues := 0
	ft.writeHook = func(addr uint16) (error, bool) { issues++; return nil, true }
	reqs := []WriteRequest{
		{Point: Point{Name: "ro", Area: AreaInputRegisters, Address: 0, Type: TypeUint16}, Value: uint16(1)},
		{Point: Point{Name: "bad", Area: AreaHoldingRegisters, Address: 0, Type: TypeUint32, Order: BigEndian}, Value: uint16(1)},
	}
	res, err := WriteBatch(ft, reqs, WriteConfig{})
	if err == nil || res.OK {
		t.Fatal("want failure")
	}
	if issues != 0 {
		t.Fatalf("invalid points must never be issued, got %d writes", issues)
	}
	if res.Results[0].Err == nil || res.Results[1].Err == nil {
		t.Fatal("both points must carry per-item errors")
	}
	if res.Results[0].Status != StatusInvalid || res.Results[1].Status != StatusInvalid {
		t.Fatalf("static failures must be StatusInvalid, got %s/%s",
			res.Results[0].Status, res.Results[1].Status)
	}
	if len(res.Segments) != 0 {
		t.Fatalf("no segments expected for all-invalid batch, got %d", len(res.Segments))
	}
}

func TestWriteBatchResultsOrdered(t *testing.T) {
	ft := newFakeWriteTransport()
	reqs := []WriteRequest{
		{Point: Point{Name: "z", Area: AreaCoils, Address: 9, Type: TypeBool}, Value: true},
		{Point: Point{Name: "a", Area: AreaCoils, Address: 0, Type: TypeBool}, Value: false},
		{Point: Point{Name: "m", Area: AreaCoils, Address: 5, Type: TypeBool}, Value: true},
	}
	res, err := WriteBatch(ft, reqs, WriteConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range res.Results {
		if r.Index != i || r.Point.Name != reqs[i].Point.Name {
			t.Fatalf("results not aligned with input at %d", i)
		}
	}
}

func TestWriteBatchConcurrentNoCrossTalk(t *testing.T) {
	ft := newFakeWriteTransport()
	// 不同批次使用互不相交的地址区间并发写。
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for b := 0; b < 8; b++ {
		wg.Add(1)
		go func(b int) {
			defer wg.Done()
			base := uint16(b * 10)
			reqs := []WriteRequest{
				{Point: Point{Name: "c", Area: AreaCoils, Address: base, Type: TypeBool},
					Value: b%2 == 0},
				{Point: Point{Name: "r", Area: AreaHoldingRegisters, Address: base,
					Type: TypeUint32, Order: BigEndianByteSwap}, Value: uint32(b + 1)},
			}
			_, errs[b] = WriteBatch(ft, reqs, WriteConfig{})
		}(b)
	}
	wg.Wait()
	for b, e := range errs {
		if e != nil {
			t.Fatalf("batch %d: %v", b, e)
		}
		base := uint16(b * 10)
		if ft.coils[base] != (b%2 == 0) {
			t.Fatalf("batch %d coil cross-talk: %v", b, ft.coils[base])
		}
		// uint32(b+1) CDAB 线路布局读回校验
		regs, _ := ft.ReadHoldingRegisters(base, 2)
		want, _ := EncodeValue(TypeUint32, BigEndianByteSwap, uint32(b+1))
		if regs[0] != want[0] || regs[1] != want[1] {
			t.Fatalf("batch %d registers cross-talk: %04X,%04X want %04X,%04X",
				b, regs[0], regs[1], want[0], want[1])
		}
	}
}
