// modbus-gateway 是本地验证入口:启动内存模拟从站,经 TCP/RTU/ASCII
// 三种传输形态读取一批映射点位,并演示一次"超时丢弃 -> 重试恢复"。
//
// 用法:
//
//	go run ./cmd/modbus-gateway -transport tcp|rtu|ascii
package main

import (
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/lisama569/modbus-gateway-go/pkg/mapping"
	"github.com/lisama569/modbus-gateway-go/pkg/modbus"

	gateway "github.com/lisama569/modbus-gateway-go"
)

type stdLogger struct{}

func (stdLogger) Printf(format string, args ...any) { log.Printf(format, args...) }

func main() {
	transport := flag.String("transport", "tcp", "传输形态: tcp | rtu | ascii")
	flag.Parse()

	points := []mapping.Point{
		{Name: "temperature", Area: mapping.AreaHoldingRegisters, Address: 0,
			Type: mapping.TypeFloat32, Order: mapping.BigEndian},
		{Name: "counter", Area: mapping.AreaHoldingRegisters, Address: 2,
			Type: mapping.TypeUint32, Order: mapping.LittleEndian},
		{Name: "running", Area: mapping.AreaCoils, Address: 0, Type: mapping.TypeBool},
	}

	gw, err := gateway.Start(gateway.Config{
		Transport: modbus.Transport(*transport),
		Unit:      1,
		Timeout:   500 * time.Millisecond,
		Retry:     1,
		Logger:    stdLogger{},
		Points:    points,
	}, nil)
	if err != nil {
		log.Fatalf("start gateway: %v", err)
	}
	defer gw.Close()

	slave := gw.Slave()
	// float32 36.6 的大端寄存器
	slave.SetHolding(0, 0x4212)
	slave.SetHolding(1, 0x6666)
	// DCBA 布局的 uint32 = 0x000004D2 (1234): 寄存器 [0xD204, 0x0000]
	slave.SetHolding(2, 0xD204)
	slave.SetHolding(3, 0x0000)
	slave.SetCoil(0, true)

	fmt.Printf("== 正常批量读取(transport=%s)==\n", *transport)
	res, err := gw.ReadAll()
	if err != nil {
		log.Fatalf("read all: %v", err)
	}
	for _, r := range res.Results {
		fmt.Printf("point=%-12s value=%v\n", r.Point.Name, r.Value)
	}
	for i, seg := range res.Segments {
		fmt.Printf("segment[%d] area=%d addr=%d qty=%d err=%v\n",
			i, seg.Area, seg.Address, seg.Quantity, res.SegmentErrors[i])
	}

	fmt.Println("== 注入一次请求丢弃:期望超时后重建连接并重试成功 ==")
	slave.AddInject(&modbus.InjectRule{
		Function: modbus.FCReadHoldingRegisters, Drop: true, Remaining: 1})
	res2, err := gw.ReadAll()
	if err != nil {
		log.Fatalf("retry did not recover: %v", err)
	}
	fmt.Printf("retry recovered, counter=%v, overall OK=%v\n",
		res2.Results[1].Value, res2.OK)
}
