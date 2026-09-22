// demo 在本地启动模拟从站，用三种传输形态各跑一轮读写，
// 打印原始报文与归一化判定结果，作为本地验证入口。
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/lisama569/modbus-gateway-go/modbus"
)

func main() {
	for _, tr := range []modbus.Transport{
		modbus.TransportTCP, modbus.TransportRTU, modbus.TransportASCII,
	} {
		if err := run(tr); err != nil {
			log.Fatalf("[%s] %v", tr, err)
		}
	}
	fmt.Println("demo: all transports OK")
}

func run(tr modbus.Transport) error {
	mem := modbus.NewSlaveMemory(64)
	mem.SetRegister(0, 0x0042)
	mem.SetRegister(2, 0x3F80) // float32 1.0 高位（ABCD）
	mem.SetRegister(3, 0x0000)
	mem.SetCoil(1, true)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	slave, err := modbus.StartSlave(ln, modbus.SlaveConfig{
		Transport: tr, Memory: mem, UnitID: 1,
	})
	if err != nil {
		return err
	}
	defer slave.Close()

	sess, err := modbus.NewSession(modbus.SessionConfig{
		Transport: tr,
		UnitID:    1,
		Dial: func(ctx context.Context) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, "tcp", slave.Addr().String())
		},
		Timeout:  time.Second,
		MaxRetry: 2,
	})
	if err != nil {
		return err
	}
	defer sess.Close()
	client := modbus.NewClient(sess)

	ctx := context.Background()
	regs, err := client.ReadRegisters(ctx, modbus.RegionHoldingRegisters, 0, 4)
	if err != nil {
		return err
	}
	bits, err := client.ReadBits(ctx, modbus.RegionCoils, 0, 4)
	if err != nil {
		return err
	}
	if err := client.WriteSingleRegister(ctx, 10, 0xBEEF); err != nil {
		return err
	}

	points := []modbus.Point{
		{Name: "temp", Region: modbus.RegionHoldingRegisters, Address: 0, Type: modbus.TypeUint16},
		{Name: "flow", Region: modbus.RegionHoldingRegisters, Address: 2, Type: modbus.TypeFloat32, Order: modbus.OrderABCD},
		{Name: "valve", Region: modbus.RegionCoils, Address: 1},
	}
	res, err := modbus.BatchRead(ctx, client, points)
	if err != nil {
		return err
	}
	fmt.Printf("[%s] regs=%04X bits=%v batch: temp=%v flow=%v valve=%v stats=%+v\n",
		tr, regs, bits,
		res.Items["temp"].Value, res.Items["flow"].Value, res.Items["valve"].Value,
		sess.Stats())
	return nil
}
