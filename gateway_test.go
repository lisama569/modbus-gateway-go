package gateway

import (
	"testing"
	"time"

	"github.com/lisama569/modbus-gateway-go/pkg/mapping"
	"github.com/lisama569/modbus-gateway-go/pkg/modbus"
)

type tlogger struct{ t *testing.T }

func (l tlogger) Printf(format string, args ...any) { l.t.Helper(); l.t.Logf(format, args...) }

func TestEndToEndBatchRead(t *testing.T) {
	points := []mapping.Point{
		{Name: "temp", Area: mapping.AreaHoldingRegisters, Address: 0,
			Type: mapping.TypeFloat32, Order: mapping.BigEndian},
		{Name: "count", Area: mapping.AreaHoldingRegisters, Address: 2,
			Type: mapping.TypeUint16},
		{Name: "run", Area: mapping.AreaCoils, Address: 0, Type: mapping.TypeBool},
	}
	gw, err := Start(Config{
		Transport: modbus.TransportTCP,
		Unit:      1,
		Timeout:   300 * time.Millisecond,
		Retry:     0,
		Logger:    tlogger{t},
		Points:    points,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	slave := gw.Slave()
	slave.SetHolding(0, 0x4049) // float32 3.1415927
	slave.SetHolding(1, 0x0FDB)
	slave.SetHolding(2, 77)
	slave.SetCoil(0, true)

	res, err := gw.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Succeeded != 3 {
		t.Fatalf("result not ok: %+v", res)
	}
	temp := res.Results[0].Value.(float32)
	if temp < 3.14 || temp > 3.15 {
		t.Fatalf("temp=%v", temp)
	}
	if res.Results[1].Value.(uint16) != 77 {
		t.Fatalf("count=%v", res.Results[1].Value)
	}
	if res.Results[2].Value.(bool) != true {
		t.Fatalf("run=%v", res.Results[2].Value)
	}
	t.Logf("end-to-end values: temp=%v count=%v run=%v",
		temp, res.Results[1].Value, res.Results[2].Value)
}

func TestEndToEndAllTransports(t *testing.T) {
	for _, tr := range []modbus.Transport{
		modbus.TransportTCP, modbus.TransportRTU, modbus.TransportASCII} {
		points := []mapping.Point{
			{Name: "v", Area: mapping.AreaHoldingRegisters, Address: 10,
				Type: mapping.TypeUint32, Order: mapping.LittleEndian},
		}
		gw, err := Start(Config{Transport: tr, Unit: 1,
			Timeout: 300 * time.Millisecond, Points: points}, nil)
		if err != nil {
			t.Fatal(err)
		}
		// DCBA: 值 0x12345678,寄存器 [0x7856,0x3412]
		gw.Slave().SetHolding(10, 0x7856)
		gw.Slave().SetHolding(11, 0x3412)
		res, err := gw.ReadAll()
		if err != nil {
			gw.Close()
			t.Fatalf("%s: %v", tr, err)
		}
		if res.Results[0].Value.(uint32) != 0x12345678 {
			t.Fatalf("%s: %08X", tr, res.Results[0].Value)
		}
		gw.Close()
	}
}

func TestEndToEndTimeoutRetryRecovers(t *testing.T) {
	points := []mapping.Point{
		{Name: "v", Area: mapping.AreaHoldingRegisters, Address: 0, Type: mapping.TypeUint16},
	}
	gw, err := Start(Config{Transport: modbus.TransportRTU, Unit: 1,
		Timeout: 300 * time.Millisecond, Retry: 1, Logger: tlogger{t}, Points: points}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	gw.Slave().SetHolding(0, 0xAAAA)
	// 首次丢弃 -> 超时 -> 重建连接 -> 成功。
	gw.Slave().AddInject(&modbus.InjectRule{
		Function: modbus.FCReadHoldingRegisters, Drop: true, Remaining: 1})
	res, err := gw.ReadAll()
	if err != nil {
		t.Fatalf("expected recovery after retry: %v", err)
	}
	if res.Results[0].Value.(uint16) != 0xAAAA {
		t.Fatalf("value=%v", res.Results[0].Value)
	}
	t.Logf("gateway recovered after timeout-retry: v=%x", res.Results[0].Value)
}
