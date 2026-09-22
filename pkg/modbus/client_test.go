package modbus

import (
	"sync"
	"testing"
	"time"
)

type testLogger struct{ t *testing.T }

func (l testLogger) Printf(format string, args ...any) {
	l.t.Helper()
	l.t.Logf(format, args...)
}

func startGateway(t *testing.T, tr Transport, retry int) (*Server, *Client, *Slave) {
	t.Helper()
	slave := NewSlave(65536)
	for i := uint16(0); i < 10; i++ {
		slave.SetHolding(i, 0x1000+i)
	}
	slave.SetCoil(0, true)
	server, err := ServeLocal(tr, slave)
	if err != nil {
		t.Fatal(err)
	}
	client, err := Dial(ClientConfig{
		Transport: tr,
		Addr:      server.Addr(),
		Unit:      1,
		Timeout:   300 * time.Millisecond,
		Retry:     retry,
		Logger:    testLogger{t},
	})
	if err != nil {
		t.Fatal(err)
	}
	return server, client, slave
}

func TestClientReadAllTransports(t *testing.T) {
	for _, tr := range []Transport{TransportTCP, TransportRTU, TransportASCII} {
		server, client, _ := startGateway(t, tr, 0)
		regs, err := client.ReadHoldingRegisters(0, 5)
		if err != nil {
			t.Fatalf("%s: %v", tr, err)
		}
		for i, v := range regs {
			if v != 0x1000+uint16(i) {
				t.Fatalf("%s reg[%d]=0x%04X", tr, i, v)
			}
		}
		bits, err := client.ReadCoils(0, 1)
		if err != nil || !bits[0] {
			t.Fatalf("%s coil: %v %v", tr, bits, err)
		}
		client.Close()
		server.Close()
	}
}

func TestExceptionResponseNotRetried(t *testing.T) {
	for _, tr := range []Transport{TransportTCP, TransportRTU, TransportASCII} {
		server, client, slave := startGateway(t, tr, 3)
		slave.AddInject(&InjectRule{Function: FCReadHoldingRegisters,
			Exception: ExcIllegalDataAddress, Remaining: -1})
		before := slave.RequestCount()
		_, err := client.ReadHoldingRegisters(0, 2)
		if KindOf(err) != KindExceptionResponse {
			t.Fatalf("%s: want exception, got %v", tr, err)
		}
		if got := slave.RequestCount() - before; got != 1 {
			t.Fatalf("%s: exception request tried %d times, want 1 (no retry)", tr, got)
		}
		t.Logf("[%s] exception normalized: %s code=0x%X",
			tr, err, byte(err.(*Error).ExceptionCode))
		client.Close()
		server.Close()
	}
}

func TestTimeoutThenRetrySucceeds(t *testing.T) {
	for _, tr := range []Transport{TransportTCP, TransportRTU, TransportASCII} {
		server, client, slave := startGateway(t, tr, 2)
		// 第一次丢弃请求,触发超时;第二次成功。
		slave.AddInject(&InjectRule{Function: FCReadHoldingRegisters, Drop: true, Remaining: 1})
		regs, err := client.ReadHoldingRegisters(0, 2)
		if err != nil {
			t.Fatalf("%s: expected retry success, got %v", tr, err)
		}
		if len(regs) != 2 {
			t.Fatalf("%s: %v", tr, regs)
		}
		t.Logf("[%s] timeout retry normalized: OK after reconnect, regs=%x", tr, regs)
		client.Close()
		server.Close()
	}
}

func TestTimeoutAllAttemptsExhausted(t *testing.T) {
	for _, tr := range []Transport{TransportTCP, TransportRTU, TransportASCII} {
		server, client, slave := startGateway(t, tr, 1)
		slave.AddInject(&InjectRule{Drop: true, Remaining: -1})
		start := time.Now()
		_, err := client.ReadHoldingRegisters(0, 1)
		elapsed := time.Since(start)
		if KindOf(err) != KindTimeout {
			t.Fatalf("%s: want timeout, got %v", tr, err)
		}
		if elapsed < 500*time.Millisecond {
			t.Fatalf("%s: returned too fast %v (expected 2 attempts)", tr, elapsed)
		}
		t.Logf("[%s] retries exhausted normalized: %s, elapsed=%v", tr, err, elapsed)
		client.Close()
		server.Close()
	}
}

func TestConnectionReconnectAfterServerRestart(t *testing.T) {
	for _, tr := range []Transport{TransportTCP, TransportRTU, TransportASCII} {
		slave := NewSlave(65536)
		slave.SetHolding(0, 0x4242)
		server, err := ServeLocal(tr, slave)
		if err != nil {
			t.Fatal(err)
		}
		client, err := Dial(ClientConfig{Transport: tr, Addr: server.Addr(),
			Unit: 1, Timeout: 300 * time.Millisecond, Retry: 2, Logger: testLogger{t}})
		if err != nil {
			t.Fatal(err)
		}
		// 首次读取正常
		if v, err := client.ReadHoldingRegisters(0, 1); err != nil || v[0] != 0x4242 {
			t.Fatalf("first read: %v %v", v, err)
		}
		// 关闭服务端,模拟连接失效;随后用相同地址重启服务端。
		addr := server.Addr()
		server.Close()
		server2, err := ServeLocalAt(tr, slave, addr)
		if err != nil {
			t.Fatal(err)
		}
		// 失效连接上的读取应触发重连并成功。
		v, err := client.ReadHoldingRegisters(0, 1)
		if err != nil || v[0] != 0x4242 {
			t.Fatalf("%s reconnect read: %v %v", tr, v, err)
		}
		t.Logf("[%s] connection rebuilt normalized: OK, reg=%x", tr, v)
		client.Close()
		server2.Close()
	}
}

func TestChecksumFailureReconnect(t *testing.T) {
	// RTU 破坏 CRC:第一次必失败,重试在新连接上成功。
	server, client, slave := startGateway(t, TransportRTU, 1)
	slave.AddInject(&InjectRule{Function: FCReadHoldingRegisters,
		CorruptChecksum: true, Remaining: 1})
	v, err := client.ReadHoldingRegisters(0, 1)
	if err != nil || v[0] != 0x1000 {
		t.Fatalf("retry after crc failure: %v %v", v, err)
	}
	t.Logf("[rtu] crc failure then reconnect normalized: OK")
	client.Close()
	server.Close()
}

func TestLengthMismatchReconnect(t *testing.T) {
	server, client, slave := startGateway(t, TransportTCP, 1)
	slave.AddInject(&InjectRule{Function: FCReadHoldingRegisters,
		CorruptLength: true, Remaining: 1})
	v, err := client.Do(&Request{Function: FCReadHoldingRegisters,
		Address: 0, Quantity: 1})
	if err != nil || v.Registers[0] != 0x1000 {
		t.Fatalf("retry after length corruption: %v %v", v, err)
	}
	t.Logf("[tcp] length mismatch then reconnect normalized: OK")
	client.Close()
	server.Close()
}

func TestTCPLengthMismatchClassified(t *testing.T) {
	server, client, slave := startGateway(t, TransportTCP, 0)
	slave.AddInject(&InjectRule{Function: FCReadHoldingRegisters,
		CorruptLength: true, Remaining: -1})
	_, err := client.ReadHoldingRegisters(0, 1)
	if KindOf(err) != KindLengthMismatch {
		t.Fatalf("want KindLengthMismatch, got kind=%d err=%v", KindOf(err), err)
	}
	t.Logf("[tcp] declared>actual normalized: %s", err)
	client.Close()
	server.Close()
}

func TestTCPDropIsTimeoutNotLengthMismatch(t *testing.T) {
	server, client, slave := startGateway(t, TransportTCP, 0)
	slave.AddInject(&InjectRule{Function: FCReadHoldingRegisters, Drop: true, Remaining: -1})
	_, err := client.ReadHoldingRegisters(0, 1)
	if KindOf(err) != KindTimeout {
		t.Fatalf("want KindTimeout for dropped frame, got kind=%d err=%v", KindOf(err), err)
	}
	t.Logf("[tcp] dropped frame normalized: %s", err)
	client.Close()
	server.Close()
}

func TestConcurrentRequestsStayOrdered(t *testing.T) {
	for _, tr := range []Transport{TransportTCP, TransportRTU, TransportASCII} {
		server, client, _ := startGateway(t, tr, 0)
		const n = 40
		var wg sync.WaitGroup
		errs := make([]error, n)
		wg.Add(n)
		for i := 0; i < n; i++ {
			i := i
			go func() {
				defer wg.Done()
				addr := uint16(i % 10)
				v, err := client.ReadHoldingRegisters(addr, 1)
				if err == nil && v[0] != 0x1000+addr {
					err = &Error{Kind: KindTransactionMismatch,
						Msg: "concurrent response cross-matched"}
				}
				errs[i] = err
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("%s concurrent request %d: %v", tr, i, err)
			}
		}
		t.Logf("[%s] %d concurrent requests normalized: all correctly paired", tr, n)
		client.Close()
		server.Close()
	}
}

func TestWrites(t *testing.T) {
	server, client, slave := startGateway(t, TransportTCP, 0)
	if err := client.WriteSingleRegister(100, 0xBEEF); err != nil {
		t.Fatal(err)
	}
	if v, _ := slave.Holding(100); v != 0xBEEF {
		t.Fatalf("holding 100 = 0x%X", v)
	}
	if err := client.WriteMultipleCoils(50, []bool{true, false, true, true}); err != nil {
		t.Fatal(err)
	}
	if on, _ := slave.Coil(50); !on {
		t.Fatal("coil 50 should be on")
	}
	if on, _ := slave.Coil(51); on {
		t.Fatal("coil 51 should be off")
	}
	if err := client.WriteMultipleRegisters(60, []uint16{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if v, _ := slave.Holding(62); v != 3 {
		t.Fatalf("holding 62 = %d", v)
	}
	client.Close()
	server.Close()
}
