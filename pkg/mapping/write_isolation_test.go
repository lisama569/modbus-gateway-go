package mapping

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lisama569/modbus-gateway-go/pkg/modbus"
)

// instrumentedTransport 在 fakeWriteTransport 之上增加读回钩子与事件流水,
// 用于确定性复现"多批并发写同一区域"的交错时序。
type instrumentedTransport struct {
	*fakeWriteTransport
	mu       sync.Mutex
	events   []string
	readHook func(area Area, addr, qty uint16)
}

func newInstrumentedTransport() *instrumentedTransport {
	return &instrumentedTransport{fakeWriteTransport: newFakeWriteTransport()}
}

func (t *instrumentedTransport) log(ev string) {
	t.mu.Lock()
	t.events = append(t.events, ev)
	t.mu.Unlock()
}

func (t *instrumentedTransport) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.events))
	copy(out, t.events)
	return out
}

func (t *instrumentedTransport) ReadCoils(addr, qty uint16) ([]bool, error) {
	if t.readHook != nil {
		t.readHook(AreaCoils, addr, qty)
	}
	return t.fakeWriteTransport.ReadCoils(addr, qty)
}

func (t *instrumentedTransport) ReadHoldingRegisters(addr, qty uint16) ([]uint16, error) {
	if t.readHook != nil {
		t.readHook(AreaHoldingRegisters, addr, qty)
	}
	return t.fakeWriteTransport.ReadHoldingRegisters(addr, qty)
}

// TestWriterOverlappingBatchesAtomicIssueConfirm 确定性复现并验证:
// 批次 A 在同一批的"下发(超时但已生效)—读回确认"恢复过程中,
// 覆盖同一区间的批次 B 不得插入下发,否则 A 读回会确认成 B 的值,
// 且 B 刚写成功的值会被 A 的重发覆盖。
func TestWriterOverlappingBatchesAtomicIssueConfirm(t *testing.T) {
	ft := newInstrumentedTransport()
	wr := NewWriter(ft)
	// 确定性时序的同步通道(全部单次触发)。
	aWriteObserved := make(chan struct{})
	bParked := make(chan struct{})
	bMayPark := make(chan struct{})
	allowATimeoutReturn := make(chan struct{})
	var confirmOnce sync.Once
	aConfirmReadDone := make(chan struct{})
	// 确定性同步点:B 已确认冲突、即将挂起等待。waitHook 返回的通道在
	// 主测试放行 A 之前不会关闭 —— B 因此一定还没消费 A 的释放事件。
	wr.locks.waitHook = func(area Area, start, end uint16) <-chan struct{} {
		ft.log("B:waiting")
		close(bParked)
		return bMayPark
	}

	// A 的首次 FC10 下发:已落地但应答超时。
	ft.writeHook = func(addr uint16) (error, bool) {
		switch addr {
		case 0: // 批次 A 的片段(@0,2 寄存器)
			ft.log("A:write(timeout,applied)")
			aWriteObserved <- struct{}{}
			<-allowATimeoutReturn // 等 B 已到达并阻塞在区间锁上
			return timeoutErr(), true
		case 1: // 批次 B 的片段(@1,1 寄存器)
			ft.log("B:write(ok)")
			// 锁若失效,B 会在 A 读回之前下发:此处兜底检测。
			select {
			case <-aConfirmReadDone:
			default:
				t.Errorf("batch B issued before batch A confirm-read completed")
			}
			return nil, true
		}
		return nil, true
	}
	ft.readHook = func(area Area, addr, qty uint16) {
		if area == AreaHoldingRegisters && addr == 0 && qty == 2 {
			// 仅 A 恢复期间的首次读回参与时序编排(测试末尾的状态校验读忽略)。
			confirmOnce.Do(func() {
				ft.log("A:readback")
				select {
				case <-bParked:
				default:
					t.Error("A confirm-read happened before B parked on the lock")
				}
				close(aConfirmReadDone)
			})
		}
	}

	reqA := []WriteRequest{
		{Point: Point{Name: "a0", Area: AreaHoldingRegisters, Address: 0,
			Type: TypeUint16}, Value: uint16(0xAAAA)},
		{Point: Point{Name: "a1", Area: AreaHoldingRegisters, Address: 1,
			Type: TypeUint16}, Value: uint16(0xAAAA)},
	}
	reqB := []WriteRequest{
		{Point: Point{Name: "b1", Area: AreaHoldingRegisters, Address: 1,
			Type: TypeUint16}, Value: uint16(0xBBBB)},
	}

	var (
		wg         sync.WaitGroup
		resA, resB *WriteBatchResult
		errA, errB error
	)
	wg.Add(2)
	go func() { defer wg.Done(); resA, errA = wr.WriteBatch(reqA, WriteConfig{MaxAttempts: 3}) }()
	go func() {
		defer wg.Done()
		<-aWriteObserved // A 的首次下发已经(带着锁)发生
		resB, errB = wr.WriteBatch(reqB, WriteConfig{MaxAttempts: 3})
	}()

	<-bParked // B 已确认冲突且尚未挂起(也尚未消费 A 的释放)
	close(allowATimeoutReturn)
	// A 完成读回确认并释放区间后,再允许 B 真正挂起/竞争 —— 时序完全确定。
	<-aConfirmReadDone
	close(bMayPark)

	wg.Wait()

	if errA != nil || !resA.OK {
		t.Fatalf("batch A must be confirmed-success: %v res=%+v", errA, resA)
	}
	if resA.Results[0].Status != StatusConfirmed || !resA.Results[0].Confirmed ||
		resA.Results[0].Attempts != 1 {
		t.Fatalf("A must be confirmed by its own value with 1 issue: %+v", resA.Results[0])
	}
	if errB != nil || !resB.OK {
		t.Fatalf("batch B must succeed after A releases: %v res=%+v", errB, resB)
	}
	if resB.Results[0].Status != StatusWritten {
		t.Fatalf("B must be an explicit write, got %s", resB.Results[0].Status)
	}
	// 最终设备状态必须是 A 写完、B 随后覆盖 addr1:两批都成功,最终值与 B 的结果一致。
	if v, _ := ft.ReadHoldingRegisters(0, 2); v[0] != 0xAAAA || v[1] != 0xBBBB {
		t.Fatalf("device final state wrong: %04X,%04X want AAAA,BBBB", v[0], v[1])
	}
	// 顺序断言:A 的下发 -> B 到达等待 -> A 读回 -> B 下发。
	got := ft.snapshot()
	want := []string{"A:write(timeout,applied)", "B:waiting", "A:readback", "B:write(ok)"}
	if len(got) != len(want) {
		t.Fatalf("events=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d]=%q want %q (full=%v)", i, got[i], want[i], got)
		}
	}
	// A 绝不能因为 B 的插入而重发:A 的物理下发只有 1 次。
	if n := ft.callCount(0); n != 1 {
		t.Fatalf("batch A must issue exactly once, got %d", n)
	}
}

// TestWriteStatusUncertainExhaustedDistinguished 验证"响应丢失且可能已生效、
// 读回也未确认、次数耗尽"必须标注为 StatusUncertain,与明确拒绝区分开,
// 且物理下发严格不超过 MaxAttempts,不再额外补写。
func TestWriteStatusUncertainExhaustedDistinguished(t *testing.T) {
	ft := newInstrumentedTransport()
	issues := 0
	ft.writeHook = func(addr uint16) (error, bool) {
		issues++
		// 每次都超时且未生效,读回自然也不匹配。
		return timeoutErr(), false
	}
	reqs := []WriteRequest{
		{Point: Point{Name: "v", Area: AreaHoldingRegisters, Address: 8,
			Type: TypeUint16}, Value: uint16(42)},
	}
	res, err := WriteBatch(ft, reqs, WriteConfig{MaxAttempts: 3})
	if err == nil || res.OK {
		t.Fatal("exhausted uncertain writes must not be reported success")
	}
	r := res.Results[0]
	if r.Status != StatusUncertain {
		t.Fatalf("want StatusUncertain, got %s", r.Status)
	}
	if !r.Uncertain() {
		t.Fatal("Uncertain() must be true")
	}
	if r.Succeeded() || r.Rejected() {
		t.Fatal("uncertain result is neither success nor rejected")
	}
	if r.Attempts != 3 {
		t.Fatalf("want 3 attempts, got %d", r.Attempts)
	}
	if issues != 3 {
		t.Fatalf("physical issues must be capped at MaxAttempts, got %d", issues)
	}
	me := &MapError{}
	if !errors.As(r.Err, &me) || me.Kind != "uncertain" {
		t.Fatalf("result error must be MapError kind=uncertain, got %#v", r.Err)
	}
	var me2 *modbus.Error
	if !errors.As(r.Err, &me2) || me2.Kind != modbus.KindTimeout {
		t.Fatalf("wrapped modbus timeout must remain on the chain: %v", r.Err)
	}
	if ft.holding[8] != 0 {
		t.Fatal("unapplied write must not be visible")
	}
}

// TestWriteStatusRejectedDefinite 验证对端明确拒绝:StatusRejected、
// 确定未生效、只下发一次、错误链保留异常码信息。
func TestWriteStatusRejectedDefinite(t *testing.T) {
	ft := newInstrumentedTransport()
	issues := 0
	ft.writeHook = func(addr uint16) (error, bool) {
		issues++
		return rejectErr(), false
	}
	reqs := []WriteRequest{
		{Point: Point{Name: "v", Area: AreaHoldingRegisters, Address: 9,
			Type: TypeUint16}, Value: uint16(1)},
	}
	res, err := WriteBatch(ft, reqs, WriteConfig{MaxAttempts: 5})
	if err == nil || res.OK {
		t.Fatal("reject must fail the batch")
	}
	r := res.Results[0]
	if r.Status != StatusRejected || !r.Rejected() {
		t.Fatalf("want StatusRejected, got %s", r.Status)
	}
	if r.Attempts != 1 || issues != 1 {
		t.Fatalf("reject must issue exactly once, attempts=%d issues=%d", r.Attempts, issues)
	}
	me := &MapError{}
	if !errors.As(r.Err, &me) || me.Kind != "transport" {
		t.Fatalf("reject must surface as kind=transport, got %#v", r.Err)
	}
	var me2 *modbus.Error
	if !errors.As(r.Err, &me2) || me2.ExceptionCode != modbus.ExcIllegalDataValue {
		t.Fatalf("exception code must remain on the chain: %v", r.Err)
	}
}

// TestWriteStatusInvalidNeverIssued 验证静态校验失败标注为 StatusInvalid 且零下发。
func TestWriteStatusInvalidNeverIssued(t *testing.T) {
	ft := newInstrumentedTransport()
	issues := 0
	ft.writeHook = func(addr uint16) (error, bool) { issues++; return nil, true }
	reqs := []WriteRequest{
		{Point: Point{Name: "ro", Area: AreaInputRegisters, Address: 0,
			Type: TypeUint16}, Value: uint16(1)},
		{Point: Point{Name: "ok", Area: AreaCoils, Address: 2, Type: TypeBool}, Value: true},
	}
	res, _ := WriteBatch(ft, reqs, WriteConfig{})
	if res.Results[0].Status != StatusInvalid {
		t.Fatalf("read-only point must be StatusInvalid, got %s", res.Results[0].Status)
	}
	if res.Results[1].Status != StatusWritten || !res.Results[1].Succeeded() {
		t.Fatalf("valid point must be StatusWritten, got %s", res.Results[1].Status)
	}
	if issues != 1 {
		t.Fatalf("exactly one physical issue expected, got %d", issues)
	}
}

// TestAreaLockSetOverlapAndDisjoint 直接验证区间锁表:
// 重叠区间必须等待,不相交区间(含同区带间隙、跨区)必须立即可得。
func TestAreaLockSetOverlapAndDisjoint(t *testing.T) {
	s := newAreaLockSet()
	rel0 := s.acquire(AreaHoldingRegisters, 0, 2)

	// 同区不相交(有间隙):必须立刻拿到。
	done := make(chan struct{})
	go func() {
		rel := s.acquire(AreaHoldingRegisters, 2, 3)
		rel()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disjoint interval [2,3) must be acquirable while [0,2) is held")
	}

	// 跨区:必须立刻拿到。
	done2 := make(chan struct{})
	go func() {
		rel := s.acquire(AreaCoils, 0, 2)
		rel()
		close(done2)
	}()
	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatal("other-area interval must be acquirable concurrently")
	}

	// 重叠:[1,3) 必须等 [0,2) 释放。
	parked := make(chan struct{})
	mayPark := make(chan struct{})
	s.waitHook = func(area Area, start, end uint16) <-chan struct{} {
		close(parked)
		return mayPark
	}
	acquiredOverlap := make(chan struct{})
	go func() {
		rel := s.acquire(AreaHoldingRegisters, 1, 3)
		close(acquiredOverlap)
		rel()
	}()
	select {
	case <-parked:
	case <-time.After(time.Second):
		t.Fatal("overlapping interval must reach the wait hook")
	}
	select {
	case <-acquiredOverlap:
		t.Fatal("overlapping interval must not be acquired before release")
	case <-time.After(50 * time.Millisecond):
	}
	rel0()
	close(mayPark)
	select {
	case <-acquiredOverlap:
	case <-time.After(time.Second):
		t.Fatal("overlapping interval must acquire after release")
	}
}

// TestWriterMultiSegmentBatchHoldsAllSegments 同一批次在同一数据区含两个
// 不连续片段(@0 与 @5..6,中间有间隙):批次必须先整批占有两个区间再执行 ——
// 与第二片段重叠的 B 即使在 A 处理第一片段期间到达,也必须等到 A 全部完成。
func TestWriterMultiSegmentBatchHoldsAllSegments(t *testing.T) {
	ft := newInstrumentedTransport()
	wr := NewWriter(ft)

	aSeg1Entered := make(chan struct{})
	bParked := make(chan struct{})
	mayPark := make(chan struct{})
	proceed := make(chan struct{})

	// A: @0=true(第一片段),@5..6=true,true(第二片段,走 FC0F)。
	// B: @5=false(单线圈 FC05),与 A 第二片段重叠,按值可区分两者的下发。
	var writeMu sync.Mutex
	var writes []string
	ft.writeHook = func(addr uint16) (error, bool) {
		writeMu.Lock()
		writes = append(writes, "issue@"+itoaLenM(int(addr)))
		writeMu.Unlock()
		if addr == 0 {
			close(aSeg1Entered)
			<-proceed // 卡在第一片段;此时 A 必须已经持有 @5..6 区间
		}
		return nil, true
	}
	wr.locks.waitHook = func(area Area, start, end uint16) <-chan struct{} {
		close(bParked)
		return mayPark
	}

	reqA := []WriteRequest{
		{Point: Point{Name: "a0", Area: AreaCoils, Address: 0, Type: TypeBool}, Value: true},
		{Point: Point{Name: "a5", Area: AreaCoils, Address: 5, Type: TypeBool}, Value: true},
		{Point: Point{Name: "a6", Area: AreaCoils, Address: 6, Type: TypeBool}, Value: true},
	}
	reqB := []WriteRequest{
		{Point: Point{Name: "b5", Area: AreaCoils, Address: 5, Type: TypeBool}, Value: false},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = wr.WriteBatch(reqA, WriteConfig{}) }()
	go func() {
		defer wg.Done()
		<-aSeg1Entered
		_, _ = wr.WriteBatch(reqB, WriteConfig{})
	}()

	<-bParked // B 已确认冲突并在测试控制下尚未竞争
	// A 还卡在第一片段:第二片段(@5..6)绝不能已被下发,否则说明锁没有整批持有。
	writeMu.Lock()
	got := append([]string(nil), writes...)
	writeMu.Unlock()
	for _, ev := range got {
		if ev != "issue@0" {
			t.Fatalf("seg2 must not issue while seg1 is in flight, events=%v", got)
		}
	}
	close(mayPark)
	close(proceed)
	wg.Wait()

	// A 完成后 B 才下发:最终 @5=false(B 最后写),@6=true(A),@0=true(A)。
	if bits, _ := ft.ReadCoils(0, 1); !bits[0] {
		t.Fatal("coil@0 must be true")
	}
	if bits, _ := ft.ReadCoils(5, 2); bits[0] || !bits[1] {
		t.Fatalf("final coils@5..6 = %v,%v want false,true", bits[0], bits[1])
	}
}
