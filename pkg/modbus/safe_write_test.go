package modbus

import (
	"sync"
	"testing"
	"time"
)

func startWriteGateway(t *testing.T, tr Transport, retry int) (*Server, *Client, *Slave) {
	t.Helper()
	slave := NewSlave(65536)
	server, err := ServeLocal(tr, slave)
	if err != nil {
		t.Fatal(err)
	}
	client, err := Dial(ClientConfig{
		Transport: tr,
		Addr:      server.Addr(),
		Unit:      1,
		Timeout:   250 * time.Millisecond,
		Retry:     retry,
		Logger:    testLogger{t},
	})
	if err != nil {
		t.Fatal(err)
	}
	return server, client, slave
}

func TestSafeWriteBasicAllTransports(t *testing.T) {
	for _, tr := range []Transport{TransportTCP, TransportRTU, TransportASCII} {
		server, client, slave := startWriteGateway(t, tr, 0)
		n, err := client.SafeWriteRegisters(100, []uint16{0x1234, 0x5678})
		if err != nil {
			t.Fatalf("%s: %v", tr, err)
		}
		if n != 1 {
			t.Fatalf("%s: attempts=%d want 1", tr, n)
		}
		if v, _ := slave.Holding(100); v != 0x1234 {
			t.Fatalf("%s holding100=%04X", tr, v)
		}
		if v, _ := slave.Holding(101); v != 0x5678 {
			t.Fatalf("%s holding101=%04X", tr, v)
		}
		m, err := client.SafeWriteCoils(50, []bool{true, false, true})
		if err != nil || m != 1 {
			t.Fatalf("%s coils: attempts=%d err=%v", tr, m, err)
		}
		if on, _ := slave.Coil(50); !on {
			t.Fatalf("%s coil50", tr)
		}
		if on, _ := slave.Coil(51); on {
			t.Fatalf("%s coil51", tr)
		}
		if on, _ := slave.Coil(52); !on {
			t.Fatalf("%s coil52", tr)
		}
		client.Close()
		server.Close()
	}
}

func TestSafeWriteDefiniteRejectNoRetry(t *testing.T) {
	server, client, slave := startWriteGateway(t, TransportTCP, 0)
	defer client.Close()
	defer server.Close()

	slave.AddInject(&InjectRule{Function: FCWriteMultipleRegisters,
		Exception: ExcIllegalDataAddress, Remaining: -1})
	before := slave.RequestCount()
	n, err := client.SafeWriteRegisters(10, []uint16{1})
	if err == nil {
		t.Fatal("expected exception")
	}
	if KindOf(err) != KindExceptionResponse {
		t.Fatalf("want exception, got kind=%d %v", KindOf(err), err)
	}
	if n != 1 {
		t.Fatalf("definite reject must not resend, attempts=%d", n)
	}
	if slave.RequestCount() != before+1 {
		t.Fatalf("exactly one physical write expected, delta=%d", slave.RequestCount()-before)
	}

	// 非法参数在本地 PDU 编码阶段即失败,不发任何报文。
	before = slave.RequestCount()
	if _, err := client.SafeWriteRegisters(10, nil); err == nil {
		t.Fatal("empty payload must fail locally")
	}
	if slave.RequestCount() != before {
		t.Fatal("local validation error must not hit the wire")
	}
}

// TestSafeWriteLostResponseConfirmedApplied:首个写响应丢失(请求已落数据),
// 重连重发 -> 成功;即使从站不回显成功,读回也应确认已生效。
func TestSafeWriteLostResponseConfirmedApplied(t *testing.T) {
	server, client, slave := startWriteGateway(t, TransportRTU, 0)
	defer client.Close()
	defer server.Close()

	// 第 1 次 FC10 丢弃:主站超时,但从站在丢弃前已经执行写入(serveData 先于响应)。
	slave.AddInject(&InjectRule{Function: FCWriteMultipleRegisters,
		Drop: true, ApplyThenDrop: true, Remaining: 1})
	n, err := client.SafeWriteRegisters(20, []uint16{0xAABB})
	if err != nil {
		t.Fatalf("write should be confirmed applied: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 physical writes (lost + resend), got %d", n)
	}
	if v, _ := slave.Holding(20); v != 0xAABB {
		t.Fatalf("holding20=%04X", v)
	}
}

// TestSafeWriteLostResponseReadbackConfirms:重发也丢,但读回能确认值已生效,
// 必须报成功而不是失败(不能把实际成功的控制量报成失败)。
func TestSafeWriteLostResponseReadbackConfirms(t *testing.T) {
	server, client, slave := startWriteGateway(t, TransportTCP, 0)
	defer client.Close()
	defer server.Close()

	// 前两次 FC10 都丢弃(请求均已生效),之后无规则;第 2 轮重发后读回 FC03 应成功。
	slave.AddInject(&InjectRule{Function: FCWriteMultipleRegisters,
		Drop: true, ApplyThenDrop: true, Remaining: 2})
	n, err := client.SafeWriteRegisters(30, []uint16{0x0102})
	if err != nil {
		t.Fatalf("read-back must confirm applied write: %v", err)
	}
	if n < 2 {
		t.Fatalf("want at least 2 attempts, got %d", n)
	}
}

// TestSafeWriteNotAppliedGetsResent:写响应丢失且值并未真正生效(NoApply+Drop),
// 读回不一致 -> 重发,最终成功。
func TestSafeWriteNotAppliedThenSucceeds(t *testing.T) {
	server, client, slave := startWriteGateway(t, TransportTCP, 0)
	defer client.Close()
	defer server.Close()

	slave.AddInject(&InjectRule{Function: FCWriteMultipleRegisters,
		Drop: true, NoApply: true, Remaining: 1})
	n, err := client.SafeWriteRegisters(40, []uint16{0x7788})
	if err != nil {
		t.Fatalf("resend should succeed after mismatch read-back: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 attempts, got %d", n)
	}
	if v, _ := slave.Holding(40); v != 0x7788 {
		t.Fatalf("holding40=%04X", v)
	}
}

// TestSafeWriteExhaustedReturnsUncertain:写始终丢失,确认读也始终失败,
// 最终必须返回 KindUncertain,不能盲报成功或确定失败。
func TestSafeWriteExhaustedReturnsUncertain(t *testing.T) {
	server, client, slave := startWriteGateway(t, TransportTCP, 0)
	defer client.Close()
	defer server.Close()

	// 对 FC10 与 FC03 全部永久丢弃。
	slave.AddInject(&InjectRule{Function: FCWriteMultipleRegisters, Drop: true, Remaining: -1})
	slave.AddInject(&InjectRule{Function: FCReadHoldingRegisters, Drop: true, Remaining: -1})
	n, err := client.SafeWriteRegisters(50, []uint16{0x2233})
	if err == nil {
		t.Fatal("want uncertain error")
	}
	if KindOf(err) != KindUncertain {
		t.Fatalf("want KindUncertain, got %d (%v)", KindOf(err), err)
	}
	if n != maxWriteConfirmRounds+1 {
		t.Fatalf("want %d physical writes, got %d", maxWriteConfirmRounds+1, n)
	}
	t.Logf("uncertain after %d attempts: %v", n, err)
}

// TestSafeWriteReadbackRejectIsUncertain:首次写丢失,读回被对端明确拒绝,
// 状态无法确认 -> 返回不确定,且不会无限重发。
func TestSafeWriteReadbackRejectIsUncertain(t *testing.T) {
	server, client, slave := startWriteGateway(t, TransportTCP, 0)
	defer client.Close()
	defer server.Close()

	// 首次写与重连后的重发都丢弃,才会进入读回确认;读回被明确拒绝。
	slave.AddInject(&InjectRule{Function: FCWriteMultipleRegisters, Drop: true, Remaining: 2})
	slave.AddInject(&InjectRule{Function: FCReadHoldingRegisters,
		Exception: ExcIllegalDataAddress, Remaining: -1})
	before := slave.RequestCount()
	n, err := client.SafeWriteRegisters(60, []uint16{0x4455})
	if err == nil {
		t.Fatal("want uncertain error")
	}
	if KindOf(err) != KindUncertain {
		t.Fatalf("want uncertain, got %d (%v)", KindOf(err), err)
	}
	if n != 2 {
		t.Fatalf("want 2 write attempts, got %d", n)
	}
	// 1 次写丢失 + 1 次重连重发丢失 + 1 次读回拒绝,随后即停,不继续盲发。
	if d := slave.RequestCount() - before; d != 3 {
		t.Fatalf("want 3 requests total, got %d", d)
	}
}

func TestSafeWriteConcurrentNoCrosstalk(t *testing.T) {
	server, client, slave := startWriteGateway(t, TransportTCP, 0)
	defer client.Close()
	defer server.Close()

	const goroutines = 16
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		wg.Add(0)
		go func() {
			defer wg.Done()
			base := uint16(g * 8)
			regs := []uint16{base + 1, base + 2}
			_, err := client.SafeWriteRegisters(base, regs)
			errs[g] = err
		}()
	}
	wg.Wait()
	for g, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", g, err)
		}
	}
	for g := 0; g < goroutines; g++ {
		base := uint16(g * 8)
		if v, _ := slave.Holding(base); v != base+1 {
			t.Fatalf("holding[%d]=%04X want %04X (crosstalk)", base, v, base+1)
		}
		if v, _ := slave.Holding(base + 1); v != base+2 {
			t.Fatalf("holding[%d]=%04X want %04X (crosstalk)", base+1, v, base+2)
		}
	}
}

func TestSafeWriteWideTypesRoundTrip(t *testing.T) {
	for _, tr := range []Transport{TransportTCP, TransportRTU, TransportASCII} {
		server, client, slave := startWriteGateway(t, tr, 0)
		// uint64 占 4 寄存器;单帧 123,仍在一帧内。
		regs := []uint16{0x0102, 0x0304, 0x0506, 0x0708}
		if _, err := client.SafeWriteRegisters(200, regs); err != nil {
			t.Fatalf("%s uint64 write: %v", tr, err)
		}
		got := uint64(0)
		for i := 0; i < 4; i++ {
			v, _ := slave.Holding(uint16(200 + i))
			got = got<<16 | uint64(v)
		}
		if got != 0x0102030405060708 {
			t.Fatalf("%s wide = %016X", tr, got)
		}
		client.Close()
		server.Close()
	}
}

func TestSafeWriteCoilsAppliedButLostConfirmed(t *testing.T) {
	server, client, slave := startWriteGateway(t, TransportTCP, 0)
	defer client.Close()
	defer server.Close()

	slave.AddInject(&InjectRule{Function: FCWriteMultipleCoils,
		Drop: true, ApplyThenDrop: true, Remaining: 2})
	n, err := client.SafeWriteCoils(70, []bool{true, true, false})
	if err != nil {
		t.Fatalf("coil lost-response must confirm applied: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 attempts, got %d", n)
	}
	if on, _ := slave.Coil(70); !on {
		t.Fatal("coil70 must be on")
	}
	if on, _ := slave.Coil(72); on {
		t.Fatal("coil72 must remain off")
	}
}
