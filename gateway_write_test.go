package gateway

import (
	"sync"
	"testing"
	"time"

	"github.com/lisama569/modbus-gateway-go/pkg/mapping"
	"github.com/lisama569/modbus-gateway-go/pkg/modbus"
)

func TestEndToEndBatchWrite(t *testing.T) {
	points := []mapping.Point{
		{Name: "setpoint", Area: mapping.AreaHoldingRegisters, Address: 0,
			Type: mapping.TypeFloat32, Order: mapping.BigEndian},
		{Name: "count", Area: mapping.AreaHoldingRegisters, Address: 2,
			Type: mapping.TypeUint32, Order: mapping.LittleEndian},
		{Name: "run", Area: mapping.AreaCoils, Address: 0, Type: mapping.TypeBool},
	}
	gw, err := Start(Config{Transport: modbus.TransportTCP, Unit: 1,
		Timeout: 300 * time.Millisecond, Points: points, Logger: tlogger{t}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	items := []mapping.WriteItem{
		{Point: points[0], Value: float32(36.6)},
		{Point: points[1], Value: uint32(1234)},
		{Point: points[2], Value: true},
	}
	res, err := gw.WriteItems(items)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Succeeded != 3 {
		t.Fatalf("write batch: %+v", res)
	}
	for i, r := range res.Results {
		if !r.Confirmed || r.Attempts != 1 {
			t.Fatalf("result[%d] attempts=%d confirmed=%v", i, r.Attempts, r.Confirmed)
		}
	}

	// 读回验证字节序往返。
	rr, err := gw.ReadPoints(points)
	if err != nil {
		t.Fatal(err)
	}
	if v := rr.Results[0].Value.(float32); v < 36.5 || v > 36.7 {
		t.Fatalf("setpoint=%v", v)
	}
	if rr.Results[1].Value.(uint32) != 1234 {
		t.Fatalf("count=%v", rr.Results[1].Value)
	}
	if rr.Results[2].Value.(bool) != true {
		t.Fatalf("run=%v", rr.Results[2].Value)
	}
}

func TestEndToEndWritePartialFailure(t *testing.T) {
	points := []mapping.Point{
		{Name: "ro", Area: mapping.AreaInputRegisters, Address: 0, Type: mapping.TypeUint16},
		{Name: "good", Area: mapping.AreaHoldingRegisters, Address: 0, Type: mapping.TypeUint16},
		{Name: "reject", Area: mapping.AreaHoldingRegisters, Address: 100, Type: mapping.TypeUint16},
	}
	// 从站只有 100 个保持寄存器:地址 100 的写必然得到非法地址异常(明确拒绝)。
	gw, err := Start(Config{Transport: modbus.TransportRTU, Unit: 1,
		Timeout: 300 * time.Millisecond, Points: points, Logger: tlogger{t},
		SlaveSize: 100}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	items := []mapping.WriteItem{
		{Point: points[0], Value: uint16(1)},  // 只读区域:plan 失败,不上线
		{Point: points[1], Value: uint16(42)}, // 成功并保留
		{Point: points[2], Value: uint16(7)},  // 对端明确拒绝:transport 失败
	}
	res, err := gw.WriteItems(items)
	if err == nil {
		t.Fatal("expected partial")
	}
	if res.OK || res.Succeeded != 1 || res.Failed != 2 {
		t.Fatalf("want 1 ok / 2 failed, got %d/%d: %+v", res.Succeeded, res.Failed, res)
	}
	if me := asMapErr(t, res.Results[0].Err); me.Kind != "plan" {
		t.Fatalf("read-only point must be plan failure, got %v", res.Results[0].Err)
	}
	if res.Results[1].Err != nil || !res.Results[1].Confirmed {
		t.Fatalf("good point must remain confirmed: %+v", res.Results[1])
	}
	if v, _ := gw.Slave().Holding(0); v != 42 {
		t.Fatalf("holding0=%04X want 42", v)
	}
	if me := asMapErr(t, res.Results[2].Err); me.Kind != "transport" {
		t.Fatalf("rejected point must be transport failure, got %v", res.Results[2].Err)
	}
	if res.Results[2].Attempts != 1 {
		t.Fatalf("definite reject must not retry, attempts=%d", res.Results[2].Attempts)
	}
	for i := range items {
		if res.Results[i].Index != i {
			t.Fatalf("result order mismatch at %d", i)
		}
	}
}

func TestEndToEndWriteRejectKeepsOtherSegment(t *testing.T) {
	points := []mapping.Point{
		{Name: "good", Area: mapping.AreaHoldingRegisters, Address: 0, Type: mapping.TypeUint16},
		{Name: "reject", Area: mapping.AreaHoldingRegisters, Address: 100, Type: mapping.TypeUint16},
	}
	gw, err := Start(Config{Transport: modbus.TransportTCP, Unit: 1,
		Timeout: 300 * time.Millisecond, Points: points, Logger: tlogger{t}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	// 精确让"地址 100 起"的 FC10 被拒:让从站地址空间只覆盖前 100 个寄存器。
	// 这里从站为 65536,改用注入规则次数:片段按地址序执行,good 在前不命中,
	// 因此用一条只生效 1 次的规则无法精确命中第二段;直接重启小从站网关。
	gw2, err := Start(Config{Transport: modbus.TransportTCP, Unit: 1,
		Timeout: 300 * time.Millisecond, Points: points, Logger: tlogger{t},
		SlaveSize: 100}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw2.Close()

	res, err := gw2.WriteItems([]mapping.WriteItem{
		{Point: points[0], Value: uint16(42)},
		{Point: points[1], Value: uint16(7)},
	})
	if err == nil {
		t.Fatal("expected partial")
	}
	if res.Succeeded != 1 || res.Failed != 1 {
		t.Fatalf("want 1/1, got %d/%d", res.Succeeded, res.Failed)
	}
	if !res.Results[0].Confirmed || res.Results[0].Err != nil {
		t.Fatalf("good segment must remain confirmed: %+v", res.Results[0])
	}
	if v, ok := gw2.Slave().Holding(0); !ok || v != 42 {
		t.Fatalf("holding0=%04X, want 42", v)
	}
	if res.Results[1].Err == nil {
		t.Fatal("rejected segment must carry error")
	}
}

func TestEndToEndWriteLostResponseConfirmed(t *testing.T) {
	points := []mapping.Point{
		{Name: "v", Area: mapping.AreaHoldingRegisters, Address: 0, Type: mapping.TypeUint16},
	}
	gw, err := Start(Config{Transport: modbus.TransportTCP, Unit: 1,
		Timeout: 250 * time.Millisecond, Points: points, Logger: tlogger{t}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	// 前两次 FC10 均"先落数据再丢响应"(请求已生效但响应丢失):
	// 第 2 轮读回 FC03 确认一致,必须把实际已写成功的控制量报为成功(读回一致即止)。
	gw.Slave().AddInject(&modbus.InjectRule{
		Function: modbus.FCWriteMultipleRegisters,
		Drop:     true, ApplyThenDrop: true, Remaining: 2})
	res, err := gw.WriteItems([]mapping.WriteItem{{Point: points[0], Value: uint16(0x6060)}})
	if err != nil {
		t.Fatalf("lost responses must be confirmed via read-back: %v", err)
	}
	if !res.OK || !res.Results[0].Confirmed {
		t.Fatalf("result not confirmed: %+v", res.Results[0])
	}
	if res.Results[0].Attempts != 2 {
		t.Fatalf("want 2 physical writes, got %d", res.Results[0].Attempts)
	}
	if v, _ := gw.Slave().Holding(0); v != 0x6060 {
		t.Fatalf("value actually applied: %04X", v)
	}
}

func TestEndToEndConcurrentWrites(t *testing.T) {
	points := []mapping.Point{
		{Name: "v", Area: mapping.AreaHoldingRegisters, Address: 0, Type: mapping.TypeUint32,
			Order: mapping.BigEndian},
	}
	gw, err := Start(Config{Transport: modbus.TransportTCP, Unit: 1,
		Timeout: 500 * time.Millisecond, Points: points, Logger: tlogger{t}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	const n = 24
	var wg sync.WaitGroup
	wg.Add(n)
	failures := make(chan error, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			// 每个 goroutine 使用互不重叠的地址区间,避免同批重叠限制。
			p := mapping.Point{Name: "v", Area: mapping.AreaHoldingRegisters,
				Address: uint16(i * 4), Type: mapping.TypeUint32, Order: mapping.BigEndian}
			res, werr := gw.WriteItems([]mapping.WriteItem{
				{Point: p, Value: uint32(0xA000 + uint32(i))},
			})
			if werr != nil {
				failures <- werr
				return
			}
			if !res.Results[0].Confirmed {
				failures <- &pointError{i: i}
			}
		}()
	}
	wg.Wait()
	close(failures)
	for e := range failures {
		t.Fatalf("concurrent write failure: %v", e)
	}
	for i := 0; i < n; i++ {
		want := uint32(0xA000 + uint32(i))
		hi, _ := gw.Slave().Holding(uint16(i * 4))
		lo, _ := gw.Slave().Holding(uint16(i*4 + 1))
		got := uint32(hi)<<16 | uint32(lo)
		if got != want {
			t.Fatalf("point %d = %08X want %08X (crosstalk)", i, got, want)
		}
	}
}

func asMapErr(t *testing.T, err error) *mapping.MapError {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	me, ok := err.(*mapping.MapError)
	if !ok {
		t.Fatalf("want *mapping.MapError, got %T: %v", err, err)
	}
	return me
}

type pointError struct{ i int }

func (e *pointError) Error() string { return "unconfirmed point" }
