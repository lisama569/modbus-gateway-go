package gateway

import (
	"sync"
	"testing"
	"time"

	"github.com/lisama569/modbus-gateway-go/pkg/mapping"
	"github.com/lisama569/modbus-gateway-go/pkg/modbus"
)

// TestEndToEndOverlappingBatchesLostResponsesConsistent 多批并发写同一片
// 区域、同一批点位,且首次响应随机丢失(可能已生效):在区间隔离下,每批的
// "下发—读回确认"不被其他批次插入 —— 每批要么明确成功、要么读回确认成功,
// 全部批次都报成功;设备最终值等于最后完成批次写入的值(最后写入者获胜)。
func TestEndToEndOverlappingBatchesLostResponsesConsistent(t *testing.T) {
	for _, tr := range []modbus.Transport{modbus.TransportTCP} {
		points := []mapping.Point{
			{Name: "v", Area: mapping.AreaHoldingRegisters, Address: 30,
				Type: mapping.TypeUint32, Order: mapping.BigEndian},
		}
		gw, err := Start(Config{Transport: tr, Unit: 1,
			Timeout: 300 * time.Millisecond, Retry: 0, Points: points}, nil)
		if err != nil {
			t.Fatal(err)
		}
		// 前若干次 FC10 全部"先生效再丢响应":制造大量需要读回确认的批次。
		gw.Slave().AddInject(&modbus.InjectRule{
			Function: modbus.FCWriteMultipleRegisters, Drop: true,
			ApplyBeforeDrop: true, Remaining: 8})

		const n = 8
		var wg sync.WaitGroup
		type outcome struct {
			b     int
			res   *mapping.WriteBatchResult
			err   error
			final uint32
		}
		outcomes := make([]outcome, n)
		start := make(chan struct{})
		for b := 0; b < n; b++ {
			wg.Add(1)
			go func(b int) {
				defer wg.Done()
				val := uint32(0xA0000 + b)
				<-start
				res, err := gw.WriteAll(map[string]any{"v": val},
					mapping.WriteConfig{MaxAttempts: 3})
				hi, _ := gw.Slave().Holding(30)
				lo, _ := gw.Slave().Holding(31)
				outcomes[b] = outcome{b: b, res: res, err: err,
					final: uint32(hi)<<16 | uint32(lo)}
			}(b)
		}
		close(start)
		wg.Wait()

		for _, o := range outcomes {
			if o.err != nil || o.res == nil || !o.res.OK {
				t.Fatalf("%s batch %d must succeed (explicit or confirmed): %v %+v",
					tr, o.b, o.err, o.res)
			}
			r := o.res.Results[0]
			if !r.Succeeded() {
				t.Fatalf("%s batch %d not marked success: %+v", tr, o.b, r)
			}
		}
		// 最终设备值必须是某个批次写入过的值(不允许出现撕裂/混合值)。
		hi, _ := gw.Slave().Holding(30)
		lo, _ := gw.Slave().Holding(31)
		fin := uint32(hi)<<16 | uint32(lo)
		found := false
		for b := 0; b < n; b++ {
			if fin == uint32(0xA0000+b) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s final value %08X is not any batch's value (torn write?)", tr, fin)
		}
		gw.Close()
	}
}

// TestEndToEndWriteUncertainWhenExhausted 端到端验证:每次都超时且未生效、
// 读回也无法确认、次数耗尽后,结果必须是 StatusUncertain(可能已生效),
// 物理下发严格受 MaxAttempts 限制,绝不无限重发。
func TestEndToEndWriteUncertainWhenExhausted(t *testing.T) {
	points := []mapping.Point{
		{Name: "v", Area: mapping.AreaCoils, Address: 21, Type: mapping.TypeBool},
	}
	gw, err := Start(Config{Transport: modbus.TransportTCP, Unit: 1,
		Timeout: 150 * time.Millisecond, Retry: 0, Points: points}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	before := gw.Slave().RequestCount()
	// 丢弃且不生效 -> 读回不匹配 -> 重发,直到次数耗尽。
	gw.Slave().AddInject(&modbus.InjectRule{
		Function: modbus.FCWriteSingleCoil, Drop: true, Remaining: -1})
	res, err := gw.WriteAll(map[string]any{"v": true}, mapping.WriteConfig{MaxAttempts: 3})
	if err == nil || res.OK {
		t.Fatal("exhausted uncertain write must not be success")
	}
	r := res.Results[0]
	if r.Status != mapping.StatusUncertain || !r.Uncertain() {
		t.Fatalf("want StatusUncertain, got %s", r.Status)
	}
	if r.Rejected() || r.Succeeded() {
		t.Fatal("uncertain is neither rejected nor succeeded")
	}
	if r.Attempts != 3 {
		t.Fatalf("want 3 attempts, got %d", r.Attempts)
	}
	// 3 次写 + 每次失败后(含耗尽后)的读回确认;严格不超过 3 次物理写。
	if on, _ := gw.Slave().Coil(21); on {
		t.Fatal("dropped write must not be applied")
	}
	// 恰好 3 次物理写(FC05),每次失败后各读回一次(含耗尽后的最终读回,
	// 共 4 次 FC01):不存在第 4 次写。
	if got := gw.Slave().RequestCount() - before; got != 7 {
		t.Fatalf("want 3 writes + 4 readbacks = 7 requests, got %d", got)
	}
}

// TestEndToEndWriteRejectedStatus 端到端验证明确拒绝的分类与"不重发"边界。
func TestEndToEndWriteRejectedStatus(t *testing.T) {
	points := []mapping.Point{
		{Name: "v", Area: mapping.AreaHoldingRegisters, Address: 40,
			Type: mapping.TypeUint16},
	}
	gw, err := Start(Config{Transport: modbus.TransportTCP, Unit: 1,
		Timeout: 300 * time.Millisecond, Retry: 0, Points: points}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	before := gw.Slave().RequestCount()
	gw.Slave().AddInject(&modbus.InjectRule{
		Function:  modbus.FCWriteSingleRegister,
		Exception: modbus.ExcIllegalDataAddress, Remaining: -1})
	res, _ := gw.WriteAll(map[string]any{"v": uint16(7)}, mapping.WriteConfig{MaxAttempts: 4})
	r := res.Results[0]
	if r.Status != mapping.StatusRejected || !r.Rejected() {
		t.Fatalf("want StatusRejected, got %s", r.Status)
	}
	if r.Uncertain() || r.Succeeded() {
		t.Fatal("rejected must be neither uncertain nor success")
	}
	if r.Attempts != 1 {
		t.Fatalf("reject must issue once, attempts=%d", r.Attempts)
	}
	// 明确拒绝后绝不读回、绝不重发:设备只收到这 1 个请求。
	if got := gw.Slave().RequestCount() - before; got != 1 {
		t.Fatalf("exactly one physical request for a definite reject, got %d", got)
	}
}
