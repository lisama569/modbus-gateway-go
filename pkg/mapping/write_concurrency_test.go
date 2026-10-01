package mapping

import (
	"sync"
	"testing"
)

// scriptedCore 在 fakeWriteTransport 之上按"批次 + 该批次内下发序号"
// 脚本化每次下发结果,并记录每次物理操作(写/读)所属批次的交错序列。
// 它自己实现一把批次锁,配合 batchView 在"真正持锁"时标记当前批次。
type scriptedCore struct {
	fakeWriteTransport
	// script(batch, addr, callNo) -> (err, applied);callNo 从 1 起。
	script func(batch int, addr uint16, callNo int) (error, bool)

	batchMu sync.Mutex
	mu      sync.Mutex
	log     []string // "w:addr:batch" / "r:addr:batch"
	current int      // 仅在持有 batchMu 期间有效
}

func newScriptedCore() *scriptedCore {
	c := &scriptedCore{
		fakeWriteTransport: *newFakeWriteTransport(),
		current:            -1,
	}
	return c
}

func (c *scriptedCore) lockAs(b int) {
	c.batchMu.Lock()
	c.mu.Lock()
	c.current = b
	c.mu.Unlock()
}

func (c *scriptedCore) unlock() {
	c.mu.Lock()
	c.current = -1
	c.mu.Unlock()
	c.batchMu.Unlock()
}

func (c *scriptedCore) snapshotLog() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.log...)
}

func (c *scriptedCore) record(op string, addr uint16) {
	c.mu.Lock()
	b := c.current
	c.log = append(c.log, op+":"+itoaLenM(int(addr))+":"+itoaLenM(b))
	c.mu.Unlock()
}

func (c *scriptedCore) issue(addr uint16, apply func()) error {
	c.record("w", addr)
	c.mu.Lock()
	b := c.current
	c.calls[addr]++
	n := c.calls[addr]
	c.mu.Unlock()
	err, applied := error(nil), true
	if c.script != nil {
		err, applied = c.script(b, addr, n)
	}
	c.mu.Lock()
	if err == nil || applied {
		apply()
	}
	c.mu.Unlock()
	return err
}

func (c *scriptedCore) WriteSingleRegister(addr, value uint16) error {
	return c.issue(addr, func() { c.holding[addr] = value })
}
func (c *scriptedCore) WriteMultipleRegisters(addr uint16, regs []uint16) error {
	return c.issue(addr, func() {
		for i, v := range regs {
			c.holding[addr+uint16(i)] = v
		}
	})
}
func (c *scriptedCore) ReadHoldingRegisters(addr, qty uint16) ([]uint16, error) {
	c.record("r", addr)
	return c.fakeWriteTransport.ReadHoldingRegisters(addr, qty)
}

// batchView 是某一批次独占的传输视图:嵌入同一 core 共享设备内存,
// 但用自己实现的 BatchGuard 在持锁瞬间声明批次身份,
// 使锁内所有 w/r 记录都带上正确的批次标签。
type batchView struct {
	*scriptedCore
	id int
}

func (v batchView) LockBatch()   { v.scriptedCore.lockAs(v.id) }
func (v batchView) UnlockBatch() { v.scriptedCore.unlock() }

// TestWriteBatchSameRegionSerializesIssueAndConfirm 复现并验证修复:
// 两批写同一地址,其中一批经历"超时(未生效)-> 读回不匹配 -> 重发成功"。
// 若批次间不隔离,两批的 w/r 会交错,恢复重发可能覆盖另一批刚写成功的值。
func TestWriteBatchSameRegionSerializesIssueAndConfirm(t *testing.T) {
	core := newScriptedCore()
	// 批次 0 的第一次写超时且未生效 -> 读回 + 重发;批次 1 一次成功。
	core.script = func(batch int, addr uint16, callNo int) (error, bool) {
		if batch == 0 && callNo == 1 {
			return timeoutErr(), false
		}
		return nil, true
	}

	const addr = uint16(0)
	req := func(v uint16) []WriteRequest {
		return []WriteRequest{
			{Point: Point{Name: "v", Area: AreaHoldingRegisters,
				Address: addr, Type: TypeUint16}, Value: v},
		}
	}

	var wg sync.WaitGroup
	res := make([]*WriteBatchResult, 2)
	errs := make([]error, 2)
	run := func(b int, v uint16) {
		defer wg.Done()
		res[b], errs[b] = WriteBatch(batchView{scriptedCore: core, id: b},
			req(v), WriteConfig{MaxAttempts: 3})
	}
	wg.Add(2)
	go run(0, 0xAAAA)
	go run(1, 0xBBBB)
	wg.Wait()

	for b := 0; b < 2; b++ {
		if errs[b] != nil || !res[b].OK {
			t.Fatalf("batch %d must succeed: err=%v res=%+v", b, errs[b], res[b])
		}
	}

	// 不变量:先跑完的批次,其全部 w/r 操作必须连续出现在日志前部,
	// 一旦切换到另一批次,就不能再出现先跑批次的操作(不可交错)。
	log := core.snapshotLog()
	if len(log) == 0 {
		t.Fatal("no operations recorded")
	}
	first := log[0][len(log[0])-1:]
	switched := false
	for _, op := range log {
		b := op[len(op)-1:]
		switch {
		case b == first && !switched:
		case b != first && !switched:
			switched = true
		case b == first && switched:
			t.Fatalf("batch interleaving detected: %v", log)
		}
	}
	seen := map[string]bool{}
	for _, op := range log {
		seen[op[len(op)-1:]] = true
	}
	if !seen["0"] || !seen["1"] {
		t.Fatalf("both batches must issue writes, log=%v", log)
	}

	// 两批都报成功:设备最终值必须是二者目标之一(不能是串台产生的其他值)。
	final := core.holding[addr]
	if final != 0xAAAA && final != 0xBBBB {
		t.Fatalf("final device value corrupted: %04X", final)
	}
}

// TestWriteBatchUncertainStatusAfterExhaustion 验证超时类不确定失败耗尽后
// 标注 StatusUncertain(可能已生效),且物理下发不超过 MaxAttempts。
func TestWriteBatchUncertainStatusAfterExhaustion(t *testing.T) {
	core := newScriptedCore()
	core.script = func(batch int, addr uint16, callNo int) (error, bool) {
		return timeoutErr(), false // 始终丢失、始终未生效
	}
	reqs := []WriteRequest{
		{Point: Point{Name: "v", Area: AreaHoldingRegisters, Address: 0, Type: TypeUint16},
			Value: uint16(0x1111)},
	}
	res, err := WriteBatch(batchView{scriptedCore: core, id: 0},
		reqs, WriteConfig{MaxAttempts: 3})
	if err == nil || res.OK {
		t.Fatal("want failure")
	}
	r := res.Results[0]
	if r.Status != StatusUncertain {
		t.Fatalf("want StatusUncertain, got %v err=%v", r.Status, r.Err)
	}
	if r.Attempts != 3 {
		t.Fatalf("physical issues must be bounded by MaxAttempts, got %d", r.Attempts)
	}
	if n := core.callCount(0); n != 3 {
		t.Fatalf("want exactly 3 writes, got %d", n)
	}
	if res.Uncertain != 1 || res.Rejected != 0 || res.Failed != 1 {
		t.Fatalf("counts wrong: rejected=%d uncertain=%d failed=%d",
			res.Rejected, res.Uncertain, res.Failed)
	}
	if me, ok := r.Err.(*MapError); !ok || me.Kind != "uncertain" {
		t.Fatalf("uncertain failure must carry uncertain MapError, got %T %v", r.Err, r.Err)
	}
}

// TestWriteBatchUncertainAppliedButUnconfirmable 验证"每次下发都可能已生效但
// 读回通道始终不可用"时,耗尽后标 uncertain(而非 rejected 或成功),
// 且下发次数受 MaxAttempts 限制。
func TestWriteBatchUncertainAppliedButUnconfirmable(t *testing.T) {
	ft := newFakeWriteTransport()
	ft.writeHook = func(addr uint16) (error, bool) {
		return timeoutErr(), true // 每次都超时但都落地(可能已生效)
	}
	rf := &readFailingTransport{inner: ft}
	reqs := []WriteRequest{
		{Point: Point{Name: "v", Area: AreaHoldingRegisters, Address: 3, Type: TypeUint16},
			Value: uint16(0x7777)},
	}
	res, err := WriteBatch(rf, reqs, WriteConfig{MaxAttempts: 2})
	if err == nil || res.OK {
		t.Fatal("want failure")
	}
	r := res.Results[0]
	if r.Status != StatusUncertain {
		t.Fatalf("applied-but-unconfirmable must be uncertain, got %v", r.Status)
	}
	if r.Attempts != 2 {
		t.Fatalf("want 2 bounded re-issues, got %d", r.Attempts)
	}
	if ft.callCount(3) != 2 {
		t.Fatalf("want 2 writes, got %d", ft.callCount(3))
	}
}

// readFailingTransport 包装 fakeWriteTransport,令读回始终失败,
// 模拟"写可能生效但确认通道不可用"。
type readFailingTransport struct{ inner *fakeWriteTransport }

func (r *readFailingTransport) ReadCoils(uint16, uint16) ([]bool, error) {
	return nil, timeoutErr()
}
func (r *readFailingTransport) ReadDiscreteInputs(uint16, uint16) ([]bool, error) {
	return nil, timeoutErr()
}
func (r *readFailingTransport) ReadHoldingRegisters(uint16, uint16) ([]uint16, error) {
	return nil, timeoutErr()
}
func (r *readFailingTransport) ReadInputRegisters(uint16, uint16) ([]uint16, error) {
	return nil, timeoutErr()
}
func (r *readFailingTransport) WriteSingleCoil(addr uint16, on bool) error {
	return r.inner.WriteSingleCoil(addr, on)
}
func (r *readFailingTransport) WriteSingleRegister(addr, v uint16) error {
	return r.inner.WriteSingleRegister(addr, v)
}
func (r *readFailingTransport) WriteMultipleCoils(addr uint16, bits []bool) error {
	return r.inner.WriteMultipleCoils(addr, bits)
}
func (r *readFailingTransport) WriteMultipleRegisters(addr uint16, regs []uint16) error {
	return r.inner.WriteMultipleRegisters(addr, regs)
}

// TestWriteBatchRejectedStatus 验证明确拒绝与静态校验失败都标 StatusRejected,
// PossibleApplied()==false,且明确拒绝只下发一次。
func TestWriteBatchRejectedStatus(t *testing.T) {
	ft := newFakeWriteTransport()
	ft.writeHook = func(addr uint16) (error, bool) { return rejectErr(), false }
	reqs := []WriteRequest{
		{Point: Point{Name: "rej", Area: AreaHoldingRegisters, Address: 0, Type: TypeUint16},
			Value: uint16(1)},
		{Point: Point{Name: "ro", Area: AreaInputRegisters, Address: 0, Type: TypeUint16},
			Value: uint16(1)},
	}
	res, err := WriteBatch(ft, reqs, WriteConfig{MaxAttempts: 4})
	if err == nil || res.OK {
		t.Fatal("want failure")
	}
	for i, r := range res.Results {
		if r.Status != StatusRejected {
			t.Fatalf("point %d want rejected, got %v", i, r.Status)
		}
		if r.PossibleApplied() {
			t.Fatalf("rejected point %d must report not-possibly-applied", i)
		}
	}
	if res.Rejected != 2 || res.Uncertain != 0 {
		t.Fatalf("counts wrong: rejected=%d uncertain=%d", res.Rejected, res.Uncertain)
	}
	if n := ft.callCount(0); n != 1 {
		t.Fatalf("rejected write issued once, got %d", n)
	}
}

// TestWriteBatchConfirmOffUncertainBounded 验证关闭读回确认时,不确定失败
// 仍按 MaxAttempts 有界重发,耗尽标 uncertain,不会无休止重发。
func TestWriteBatchConfirmOffUncertainBounded(t *testing.T) {
	ft := newFakeWriteTransport()
	ft.writeHook = func(addr uint16) (error, bool) { return timeoutErr(), false }
	reqs := []WriteRequest{
		{Point: Point{Name: "v", Area: AreaCoils, Address: 1, Type: TypeBool}, Value: true},
	}
	res, err := WriteBatch(ft, reqs, WriteConfig{MaxAttempts: 2, Confirm: ConfirmOff})
	if err == nil || res.OK {
		t.Fatal("want failure")
	}
	r := res.Results[0]
	if r.Status != StatusUncertain || r.Attempts != 2 {
		t.Fatalf("want uncertain/2 attempts, got %v/%d", r.Status, r.Attempts)
	}
	if ft.callCount(1) != 2 {
		t.Fatalf("want 2 issues, got %d", ft.callCount(1))
	}
}
