package modbus

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// startTestSlave 启动本地模拟从站并返回地址与清理函数。
func startTestSlave(t *testing.T, tr Transport, mem *SlaveMemory, hook FaultHook, unit byte) (*Slave, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sl, err := StartSlave(ln, SlaveConfig{Transport: tr, Memory: mem, UnitID: unit, Fault: hook})
	if err != nil {
		t.Fatal(err)
	}
	return sl, func() { _ = sl.Close() }
}

func dialerFor(addr string) Dialer {
	return func(ctx context.Context) (net.Conn, error) {
		d := net.Dialer{}
		return d.DialContext(ctx, "tcp", addr)
	}
}

func TestSessionReadWriteAllTransports(t *testing.T) {
	for _, tr := range []Transport{TransportTCP, TransportRTU, TransportASCII} {
		t.Run(string(tr), func(t *testing.T) {
			mem := NewSlaveMemory(100)
			mem.SetRegister(10, 0x1234)
			mem.SetCoil(5, true)
			sl, cleanup := startTestSlave(t, tr, mem, nil, 1)
			defer cleanup()

			sess, err := NewSession(SessionConfig{
				Transport: tr, UnitID: 1, Dial: dialerFor(sl.Addr().String()),
				Timeout: 2 * time.Second, MaxRetry: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			c := NewClient(sess)

			regs, err := c.ReadRegisters(context.Background(), RegionHoldingRegisters, 10, 2)
			if err != nil {
				t.Fatal(err)
			}
			if regs[0] != 0x1234 || regs[1] != 0 {
				t.Fatalf("regs=%x", regs)
			}
			bits, err := c.ReadBits(context.Background(), RegionCoils, 4, 3)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("[%s] read regs=%x bits=%v verdict=ok", tr, regs, bits)
			if !bits[1] {
				t.Fatalf("coil 5 should be on, bits=%v", bits)
			}
			if err := c.WriteSingleRegister(context.Background(), 11, 0xBEEF); err != nil {
				t.Fatal(err)
			}
			if err := c.WriteMultipleCoils(context.Background(), 20, []bool{true, false, true}); err != nil {
				t.Fatal(err)
			}
			bits2, _ := c.ReadBits(context.Background(), RegionCoils, 20, 3)
			if len(bits2) != 3 || !bits2[0] || bits2[1] || !bits2[2] {
				t.Fatalf("write coils verify failed: %v", bits2)
			}
		})
	}
}

func TestExceptionClassified(t *testing.T) {
	mem := NewSlaveMemory(10)
	sl, cleanup := startTestSlave(t, TransportTCP, mem, nil, 1)
	defer cleanup()
	sess, _ := NewSession(SessionConfig{
		Transport: TransportTCP, UnitID: 1, Dial: dialerFor(sl.Addr().String()),
		Timeout: time.Second,
	})
	defer sess.Close()

	// 读越界 -> 非法数据地址
	_, err := sess.Do(context.Background(), Request{FC: FCReadHoldingRegisters, Address: 9000, Quantity: 1})
	me := AsError(err)
	if me == nil || me.Kind != KindModbusException || me.Except != ExcIllegalDataAddress {
		t.Fatalf("want illegal_data_address, got: %v", err)
	}
	t.Logf("raw=%s -> verdict=%s exception=%s",
		strings.ToUpper(hex.EncodeToString(me.Raw)), me.Kind, me.Except)
}

func TestTimeoutRetryAndReconnect(t *testing.T) {
	mem := NewSlaveMemory(10)
	var drops int
	var mu sync.Mutex
	hook := func(adu ADU) ([]byte, bool, bool) {
		mu.Lock()
		defer mu.Unlock()
		// 前两次读请求直接丢弃（模拟无响应超时），之后恢复正常。
		if FunctionCode(adu.PDU[0]) == FCReadHoldingRegisters && drops < 2 {
			drops++
			return nil, true, true
		}
		return nil, false, false
	}
	sl, cleanup := startTestSlave(t, TransportTCP, mem, hook, 1)
	defer cleanup()
	sess, _ := NewSession(SessionConfig{
		Transport: TransportTCP, UnitID: 1, Dial: dialerFor(sl.Addr().String()),
		Timeout: 200 * time.Millisecond, MaxRetry: 3,
	})
	defer sess.Close()

	start := time.Now()
	regs, err := NewClient(sess).ReadRegisters(context.Background(), RegionHoldingRegisters, 0, 1)
	if err != nil {
		t.Fatalf("expected eventual success after retry, got %v", err)
	}
	if regs[0] != 0 {
		t.Fatalf("regs=%x", regs)
	}
	st := sess.Stats()
	if st.Timeouts < 2 || st.Reconnects < 2 || st.Retries < 2 {
		t.Fatalf("stats=%+v want >=2 timeouts/reconnects/retries", st)
	}
	t.Logf("verdict=ok-after-retry elapsed=%v stats=%+v", time.Since(start), st)
}

func TestConnectionDeathRebuild(t *testing.T) {
	mem := NewSlaveMemory(10)
	mem.SetRegister(0, 0xAAAA)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	sl, _ := StartSlave(ln, SlaveConfig{Transport: TransportRTU, Memory: mem, UnitID: 1})

	sess, _ := NewSession(SessionConfig{
		Transport: TransportRTU, UnitID: 1, Dial: dialerFor(sl.Addr().String()),
		Timeout: time.Second, MaxRetry: 1,
	})
	c := NewClient(sess)

	regs, err := c.ReadRegisters(context.Background(), RegionHoldingRegisters, 0, 1)
	if err != nil || regs[0] != 0xAAAA {
		t.Fatalf("initial read: %v %x", err, regs)
	}
	// 杀死从站：连接失效。
	_ = sl.Close()
	time.Sleep(50 * time.Millisecond)
	_, err = c.ReadRegisters(context.Background(), RegionHoldingRegisters, 0, 1)
	if AsError(err) == nil {
		t.Fatal("expected error after slave killed")
	}
	t.Logf("after-kill verdict=%s", AsError(err).Kind)

	// 在同一地址重启从站，会话应自动重建并恢复。
	ln2, _ := net.Listen("tcp", sl.Addr().String())
	sl2, _ := StartSlave(ln2, SlaveConfig{Transport: TransportRTU, Memory: mem, UnitID: 1})
	defer sl2.Close()

	regs, err = c.ReadRegisters(context.Background(), RegionHoldingRegisters, 0, 1)
	if err != nil {
		t.Fatalf("expected recovery after rebuild, got %v", err)
	}
	if regs[0] != 0xAAAA {
		t.Fatalf("regs=%x", regs)
	}
	if sess.Stats().Reconnects < 1 {
		t.Fatalf("reconnect stats wrong: %+v", sess.Stats())
	}
	t.Logf("verdict=recovered stats=%+v", sess.Stats())
}

func TestConcurrentSessionOrdering(t *testing.T) {
	mem := NewSlaveMemory(10)
	sl, cleanup := startTestSlave(t, TransportTCP, mem, nil, 1)
	defer cleanup()
	sess, _ := NewSession(SessionConfig{
		Transport: TransportTCP, UnitID: 1, Dial: dialerFor(sl.Addr().String()),
		Timeout: 2 * time.Second, MaxRetry: 0,
	})
	defer sess.Close()

	const goroutines = 32
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			val := uint16(0x1000 + i)
			ctx := context.Background()
			if err := NewClient(sess).WriteSingleRegister(ctx, 0, val); err != nil {
				errs <- err
				return
			}
			// 每个写后读必须得到某个合法写入值，且不允许出现帧错配类错误。
			regs, err := NewClient(sess).ReadRegisters(ctx, RegionHoldingRegisters, 0, 1)
			if err != nil {
				errs <- fmt.Errorf("g%d read: %w", i, err)
				return
			}
			if regs[0] < 0x1000 || regs[0] > 0x1000+goroutines-1 {
				errs <- fmt.Errorf("g%d corrupted value %x", i, regs[0])
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	st := sess.Stats()
	if st.Requests != int64(2*goroutines) {
		t.Fatalf("requests=%d want %d", st.Requests, 2*goroutines)
	}
	t.Logf("verdict=ok concurrency=%d requests=%d retries=%d", goroutines, st.Requests, st.Retries)
}

func TestUnsupportedFCNoRetry(t *testing.T) {
	mem := NewSlaveMemory(10)
	sl, cleanup := startTestSlave(t, TransportTCP, mem, nil, 1)
	defer cleanup()
	sess, _ := NewSession(SessionConfig{
		Transport: TransportTCP, UnitID: 1, Dial: dialerFor(sl.Addr().String()),
		Timeout: time.Second, MaxRetry: 3,
	})
	defer sess.Close()
	// 客户端侧直接拦截：功能码 0x77 不在支持列表。
	_, err := sess.Do(context.Background(), Request{FC: FunctionCode(0x77), Address: 0, Quantity: 1})
	me := AsError(err)
	if me == nil || me.Kind != KindUnsupportedFunction {
		t.Fatalf("got %v", err)
	}
	if sess.Stats().Retries != 0 {
		t.Fatalf("unsupported FC must not retry: %+v", sess.Stats())
	}
}
