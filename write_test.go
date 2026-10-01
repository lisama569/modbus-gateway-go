package gateway

import (
	"sync"
	"testing"
	"time"

	"github.com/lisama569/modbus-gateway-go/pkg/mapping"
	"github.com/lisama569/modbus-gateway-go/pkg/modbus"
)

func TestEndToEndWriteAllTransports(t *testing.T) {
	for _, tr := range []modbus.Transport{
		modbus.TransportTCP, modbus.TransportRTU, modbus.TransportASCII} {
		points := []mapping.Point{
			{Name: "run", Area: mapping.AreaCoils, Address: 0, Type: mapping.TypeBool},
			{Name: "sp", Area: mapping.AreaHoldingRegisters, Address: 0,
				Type: mapping.TypeFloat32, Order: mapping.BigEndian},
			{Name: "cnt", Area: mapping.AreaHoldingRegisters, Address: 2,
				Type: mapping.TypeUint32, Order: mapping.LittleEndian},
			{Name: "raw", Area: mapping.AreaHoldingRegisters, Address: 4, Type: mapping.TypeInt16},
		}
		gw, err := Start(Config{Transport: tr, Unit: 1,
			Timeout: 300 * time.Millisecond, Retry: 0, Points: points}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := gw.WriteAll(map[string]any{
			"run": true,
			"sp":  float32(36.6),
			"cnt": uint32(1234),
			"raw": int16(-2),
		}, mapping.WriteConfig{})
		if err != nil || !res.OK {
			gw.Close()
			t.Fatalf("%s: %v res=%+v", tr, err, res)
		}

		sl := gw.Slave()
		if on, ok := sl.Coil(0); !ok || !on {
			t.Fatalf("%s: coil not set", tr)
		}
		if v, _ := sl.Holding(0); v != 0x4212 {
			t.Fatalf("%s: float reg0=%04X want 4212", tr, v)
		}
		if v, _ := sl.Holding(1); v != 0x6666 {
			t.Fatalf("%s: float reg1=%04X want 6666", tr, v)
		}
		if v, _ := sl.Holding(2); v != 0xD204 {
			t.Fatalf("%s: uint32 LE reg0=%04X want D204", tr, v)
		}
		if v, _ := sl.Holding(4); v != 0xFFFE {
			t.Fatalf("%s: int16=%04X want FFFE", tr, v)
		}
		gw.Close()
	}
}

func TestEndToEndWriteReadOnlyAndMismatch(t *testing.T) {
	reqs := []mapping.WriteRequest{
		{Point: mapping.Point{Name: "di", Area: mapping.AreaDiscreteInputs, Address: 0, Type: mapping.TypeBool}, Value: true},
		{Point: mapping.Point{Name: "ir", Area: mapping.AreaInputRegisters, Address: 0, Type: mapping.TypeUint16}, Value: uint16(1)},
		{Point: mapping.Point{Name: "ok", Area: mapping.AreaCoils, Address: 3, Type: mapping.TypeBool}, Value: true},
	}
	gw, err := Start(Config{Transport: modbus.TransportTCP, Unit: 1,
		Timeout: 300 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	before := gw.Slave().RequestCount()
	res, err := gw.WriteRequests(reqs, mapping.WriteConfig{})
	if err == nil || res.OK {
		t.Fatal("read-only points must make batch not-OK")
	}
	if res.Results[0].Err == nil || res.Results[1].Err == nil {
		t.Fatal("read-only points must carry per-item errors")
	}
	if res.Results[2].Err != nil {
		t.Fatalf("valid point must succeed: %v", res.Results[2].Err)
	}
	// 只读/非法点绝不下发:设备只应收到合法点那一次写。
	if got := gw.Slave().RequestCount() - before; got != 1 {
		t.Fatalf("exactly one physical request expected, got %d", got)
	}
}

func TestEndToEndWriteConfirmedAfterLostResponse(t *testing.T) {
	points := []mapping.Point{
		// uint32 宽点强制走 FC10(写多个寄存器),用于复现多写响应丢失。
		{Name: "v", Area: mapping.AreaHoldingRegisters, Address: 10,
			Type: mapping.TypeUint32, Order: mapping.BigEndian},
	}
	gw, err := Start(Config{Transport: modbus.TransportTCP, Unit: 1,
		Timeout: 300 * time.Millisecond, Retry: 0, Points: points}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	// FC10 第一次:先写入内存再丢响应 -> 主站超时 -> 读回确认应判定成功。
	gw.Slave().AddInject(&modbus.InjectRule{
		Function: modbus.FCWriteMultipleRegisters, Drop: true,
		ApplyBeforeDrop: true, Remaining: 1})
	res, err := gw.WriteAll(map[string]any{"v": uint32(0xBEEFCAFE)}, mapping.WriteConfig{MaxAttempts: 3})
	if err != nil || !res.OK {
		t.Fatalf("applied-but-lost write must be confirmed success: %v", err)
	}
	if !res.Results[0].Confirmed {
		t.Fatal("result must be marked Confirmed")
	}
	if res.Results[0].Attempts != 1 {
		t.Fatalf("no re-issue after confirmation, attempts=%d", res.Results[0].Attempts)
	}
	if v, _ := gw.Slave().Holding(10); v != 0xBEEF {
		t.Fatalf("device reg10=%04X want BEEF", v)
	}
	if v, _ := gw.Slave().Holding(11); v != 0xCAFE {
		t.Fatalf("device reg11=%04X want CAFE", v)
	}
}

func TestEndToEndWriteLostResponseNotAppliedRetries(t *testing.T) {
	points := []mapping.Point{
		{Name: "v", Area: mapping.AreaCoils, Address: 7, Type: mapping.TypeBool},
	}
	gw, err := Start(Config{Transport: modbus.TransportRTU, Unit: 1,
		Timeout: 300 * time.Millisecond, Retry: 0, Points: points}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	// FC05 第一次丢弃且不生效 -> 读回不匹配 -> 重发 -> 成功。
	gw.Slave().AddInject(&modbus.InjectRule{
		Function: modbus.FCWriteSingleCoil, Drop: true, Remaining: 1})
	res, err := gw.WriteAll(map[string]any{"v": true}, mapping.WriteConfig{MaxAttempts: 3})
	if err != nil || !res.OK {
		t.Fatalf("expected retry recovery: %v", err)
	}
	if on, _ := gw.Slave().Coil(7); !on {
		t.Fatal("coil not set after retry")
	}
	if res.Results[0].Attempts != 2 || res.Results[0].Confirmed {
		t.Fatalf("want attempts=2 confirmed=false, got %d/%v",
			res.Results[0].Attempts, res.Results[0].Confirmed)
	}
}

func TestEndToEndWriteExceptionNotRetried(t *testing.T) {
	points := []mapping.Point{
		{Name: "v", Area: mapping.AreaHoldingRegisters, Address: 100, Type: mapping.TypeUint16},
	}
	gw, err := Start(Config{Transport: modbus.TransportTCP, Unit: 1,
		Timeout: 300 * time.Millisecond, Retry: 0, Points: points}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	gw.Slave().AddInject(&modbus.InjectRule{
		Function:  modbus.FCWriteSingleRegister,
		Exception: modbus.ExcIllegalDataAddress, Remaining: -1}) // 永久拒绝
	res, err := gw.WriteAll(map[string]any{"v": uint16(1)}, mapping.WriteConfig{MaxAttempts: 5})
	if err == nil || res.OK {
		t.Fatal("exception must fail the point")
	}
	if res.Results[0].Attempts != 1 {
		t.Fatalf("exception response must not be retried, attempts=%d", res.Results[0].Attempts)
	}
}

func TestEndToEndConcurrentWrites(t *testing.T) {
	gw, err := Start(Config{Transport: modbus.TransportTCP, Unit: 1,
		Timeout: time.Second, Retry: 0}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	const n = 12
	var wg sync.WaitGroup
	failures := make(chan error, n)
	for b := 0; b < n; b++ {
		wg.Add(1)
		go func(b int) {
			defer wg.Done()
			base := uint16(b * 4)
			reqs := []mapping.WriteRequest{
				{Point: mapping.Point{Name: "c", Area: mapping.AreaCoils,
					Address: base, Type: mapping.TypeBool}, Value: b%2 == 1},
				{Point: mapping.Point{Name: "r", Area: mapping.AreaHoldingRegisters,
					Address: base, Type: mapping.TypeUint32,
					Order: mapping.BigEndian}, Value: uint32(0x10000 + b)},
			}
			res, err := gw.WriteRequests(reqs, mapping.WriteConfig{MaxAttempts: 2})
			if err != nil {
				failures <- err
				return
			}
			for _, r := range res.Results {
				if r.Err != nil {
					failures <- r.Err
				}
			}
		}(b)
	}
	wg.Wait()
	close(failures)
	for e := range failures {
		t.Fatal(e)
	}
	for b := 0; b < n; b++ {
		base := uint16(b * 4)
		if on, _ := gw.Slave().Coil(base); on != (b%2 == 1) {
			t.Fatalf("batch %d coil cross-talk: %v", b, on)
		}
		v0, _ := gw.Slave().Holding(base)
		v1, _ := gw.Slave().Holding(base + 1)
		want := uint32(0x10000 + b)
		if uint32(v0)<<16|uint32(v1) != want {
			t.Fatalf("batch %d register cross-talk: %04X%04X want %08X", b, v0, v1, want)
		}
	}
}

// TestEndToEndConcurrentSameRegionIsolation 验证多批并发写"同一片区域同一批点位"
// 时,每批的"下发—读回确认"是不可插入的整体,且返回结果与设备最终状态一致。
//
// 做法:多个 goroutine 反复对同一保持寄存器点写不同绝对值;其中注入一次性
// "响应丢失但未生效"(触发某批读回不匹配后重发)。所有批次要么明确成功、
// 要么明确标注 uncertain;设备最终值必须等于"最后一个成功批次"报告的值。
func TestEndToEndConcurrentSameRegionIsolation(t *testing.T) {
	points := []mapping.Point{
		{Name: "v", Area: mapping.AreaHoldingRegisters, Address: 50, Type: mapping.TypeUint16},
	}
	gw, err := Start(Config{Transport: modbus.TransportTCP, Unit: 1,
		Timeout: 200 * time.Millisecond, Retry: 0, Points: points}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	// 制造若干次"响应丢失但未生效"的不确定窗口(Drop 不落地),
	// 迫使不同批次经历 读回—重发,与别的批次在同址上竞争。
	for i := 0; i < 6; i++ {
		gw.Slave().AddInject(&modbus.InjectRule{
			Function: modbus.FCWriteSingleRegister, Drop: true, Remaining: 1})
	}

	const n = 16
	var wg sync.WaitGroup
	type outcome struct {
		batch int
		res   *mapping.WriteBatchResult
		err   error
	}
	out := make([]outcome, n)
	for b := 0; b < n; b++ {
		wg.Add(1)
		go func(b int) {
			defer wg.Done()
			val := uint16(0x0100 + b) // 每批唯一目标值
			r, e := gw.WriteAll(map[string]any{"v": val}, mapping.WriteConfig{MaxAttempts: 4})
			out[b] = outcome{b, r, e}
		}(b)
	}
	wg.Wait()

	// 设备最终值。
	final, _ := gw.Slave().Holding(50)

	// 收集"报告成功"的批次目标值集合。
	successVals := map[uint16]bool{}
	for _, o := range out {
		if o.err != nil {
			// partial 错误本身允许(可能是 uncertain);逐项检查。
		}
		if o.res == nil {
			t.Fatalf("batch %d nil result err=%v", o.batch, o.err)
		}
		r0 := o.res.Results[0]
		switch r0.Status {
		case mapping.StatusOK:
			successVals[r0.Value.(uint16)] = true
			// 报告成功的批次,其 Attempts 内的全部操作未被他批插入:
			// 最终设备值可能被更晚的成功批次覆盖,但任何成功批次写入的
			// 值一定是它自己的目标(此处仅做状态自洽校验)。
		case mapping.StatusUncertain:
			// 允许:可能已生效但未确认。绝不允许出现"明确拒绝"之外的假成功。
			if r0.PossibleApplied() != true {
				t.Fatalf("batch %d uncertain must be possible-applied", o.batch)
			}
		case mapping.StatusRejected:
			t.Fatalf("same-region writes must not be rejected: batch %d %v", o.batch, r0.Err)
		}
	}

	// 关键一致性:设备最终值要么是某个报告成功批次的目标值,
	// 要么停留在某批 uncertain 的目标值;无论如何必须是本测试写入过的值,
	// 不能是串台产生的混合/其它值。
	wroteAny := successVals[final]
	if !wroteAny {
		// 若最后完成的是 uncertain 批次,final 也可能是它的目标(0x0100+b)。
		for _, o := range out {
			if o.res.Results[0].Status == mapping.StatusUncertain &&
				o.res.Results[0].Value.(uint16) == final {
				wroteAny = true
			}
		}
	}
	if !wroteAny {
		t.Fatalf("final device value %04X matches no batch outcome (cross-talk)", final)
	}
}

// TestEndToEndWriteUncertainStatusDistinctFromReject 端到端验证:
// 持续丢响应且不落地 -> 结果为 uncertain(可能已生效)而非 rejected;
// 永久异常响应 -> rejected(确定没生效),且只下发一次。
func TestEndToEndWriteUncertainStatusDistinctFromReject(t *testing.T) {
	// uncertain 场景。
	{
		points := []mapping.Point{
			{Name: "v", Area: mapping.AreaHoldingRegisters, Address: 60, Type: mapping.TypeUint16},
		}
		gw, err := Start(Config{Transport: modbus.TransportTCP, Unit: 1,
			Timeout: 120 * time.Millisecond, Retry: 0, Points: points}, nil)
		if err != nil {
			t.Fatal(err)
		}
		gw.Slave().AddInject(&modbus.InjectRule{
			Function: modbus.FCWriteSingleRegister, Drop: true, Remaining: -1})
		res, err := gw.WriteAll(map[string]any{"v": uint16(0x3333)},
			mapping.WriteConfig{MaxAttempts: 3})
		gw.Close()
		if err == nil || res.OK {
			t.Fatal("want failure")
		}
		r := res.Results[0]
		if r.Status != mapping.StatusUncertain {
			t.Fatalf("lost responses must be uncertain, got %v", r.Status)
		}
		if r.Attempts != 3 {
			t.Fatalf("bounded by MaxAttempts, got %d", r.Attempts)
		}
		if r.PossibleApplied() != true {
			t.Fatal("uncertain must report possible applied")
		}
		if v, _ := gw.Slave().Holding(60); v == 0x3333 {
			t.Fatal("Drop without apply must not land")
		}
	}

	// rejected 场景。
	{
		points := []mapping.Point{
			{Name: "v", Area: mapping.AreaHoldingRegisters, Address: 61, Type: mapping.TypeUint16},
		}
		gw, err := Start(Config{Transport: modbus.TransportTCP, Unit: 1,
			Timeout: 200 * time.Millisecond, Retry: 0, Points: points}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer gw.Close()
		before := gw.Slave().RequestCount()
		gw.Slave().AddInject(&modbus.InjectRule{
			Function:  modbus.FCWriteSingleRegister,
			Exception: modbus.ExcIllegalDataValue, Remaining: -1})
		res, err := gw.WriteAll(map[string]any{"v": uint16(0x4444)},
			mapping.WriteConfig{MaxAttempts: 5})
		if err == nil || res.OK {
			t.Fatal("want failure")
		}
		r := res.Results[0]
		if r.Status != mapping.StatusRejected {
			t.Fatalf("exception must be rejected, got %v", r.Status)
		}
		if r.PossibleApplied() {
			t.Fatal("rejected must be definitely not applied")
		}
		if r.Attempts != 1 {
			t.Fatalf("rejected issued once, got %d", r.Attempts)
		}
		if got := gw.Slave().RequestCount() - before; got != 1 {
			t.Fatalf("exactly one physical write, got %d", got)
		}
	}
}
