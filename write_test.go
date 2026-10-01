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
	if res.Results[0].Status != mapping.StatusConfirmed {
		t.Fatalf("want StatusConfirmed, got %s", res.Results[0].Status)
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
