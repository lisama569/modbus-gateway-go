package modbus

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
)

// SlaveMemory 是模拟从站的数据区存储。
type SlaveMemory struct {
	mu               sync.Mutex
	Coils            []bool
	DiscreteInputs   []bool
	HoldingRegisters []uint16
	InputRegisters   []uint16
}

// NewSlaveMemory 创建四个数据区均为 size 个元素的存储。
func NewSlaveMemory(size int) *SlaveMemory {
	return &SlaveMemory{
		Coils:            make([]bool, size),
		DiscreteInputs:   make([]bool, size),
		HoldingRegisters: make([]uint16, size),
		InputRegisters:   make([]uint16, size),
	}
}

// SnapshotCoils 等辅助方法便于测试预置数据。
func (m *SlaveMemory) SetCoil(addr uint16, v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if int(addr) < len(m.Coils) {
		m.Coils[addr] = v
	}
}

func (m *SlaveMemory) SetRegister(addr uint16, v uint16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if int(addr) < len(m.HoldingRegisters) {
		m.HoldingRegisters[addr] = v
	}
}

func (m *SlaveMemory) SetInputRegister(addr uint16, v uint16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if int(addr) < len(m.InputRegisters) {
		m.InputRegisters[addr] = v
	}
}

func (m *SlaveMemory) SetDiscreteInput(addr uint16, v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if int(addr) < len(m.DiscreteInputs) {
		m.DiscreteInputs[addr] = v
	}
}

// errIllegalAddress 标记“地址越界”，映射为异常码 0x02（非法数据地址）。
var errIllegalAddress = NewError(KindInvalidArgument, "address out of range")

// FaultHook 允许测试注入确定性故障：返回 (响应PDU, handled, drop)。
// handled=true 表示由钩子决定响应/丢弃；drop=true 表示不回任何字节
// （用于模拟超时）；resp 为异常 PDU 或自定义响应。
type FaultHook func(adu ADU) (resp []byte, handled, drop bool)

// SlaveConfig 为模拟从站配置。
type SlaveConfig struct {
	Transport Transport
	Memory    *SlaveMemory
	UnitID    byte
	Fault     FaultHook
}

// Slave 是本地模拟从站，监听 TCP 端口并按指定传输形态收发。
type Slave struct {
	cfg    SlaveConfig
	ln     net.Listener
	codec  Codec
	pdu    PDU
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// StartSlave 在 listener 上启动模拟从站。
func StartSlave(ln net.Listener, cfg SlaveConfig) (*Slave, error) {
	codec, err := NewCodec(cfg.Transport)
	if err != nil {
		return nil, err
	}
	if cfg.Memory == nil {
		return nil, NewError(KindInvalidArgument, "slave memory is nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Slave{cfg: cfg, ln: ln, codec: codec, pdu: NewPDU(), cancel: cancel}
	s.wg.Add(1)
	go s.acceptLoop(ctx)
	return s, nil
}

// Addr 返回监听地址。
func (s *Slave) Addr() net.Addr { return s.ln.Addr() }

// Close 停止从站并关闭监听器，等待连接处理协程退出。
func (s *Slave) Close() error {
	s.cancel()
	err := s.ln.Close()
	s.wg.Wait()
	return err
}

func (s *Slave) acceptLoop(ctx context.Context) {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			return
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer c.Close()
			_ = s.ServeConn(ctx, c)
		}(conn)
	}
}

// ServeConn 在单条连接上按帧循环处理请求，严格一请求一响应。
func (s *Slave) ServeConn(ctx context.Context, conn net.Conn) error {
	// ctx 取消时主动关闭连接，解除阻塞中的 Read。
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	ft := NewFrameTransport(conn, s.codec)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		adu, err := ft.Recv(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) || AsError(err) != nil && AsError(err).Kind == KindConnClosed {
				return nil
			}
			// 损坏帧无法可靠恢复（RTU），关闭连接，迫使主站重连，
			// 避免后续帧错配。TCP 可按长度跳过，但为语义一致同样关闭。
			return err
		}
		// RTU/ASCII 非本机地址：标准串口语义为静默忽略（含广播写）。
		if s.cfg.UnitID != 0 && adu.UnitID != s.cfg.UnitID && adu.UnitID != 0 {
			continue
		}
		// 故障注入优先。
		if s.cfg.Fault != nil {
			if resp, handled, drop := s.cfg.Fault(adu); handled {
				if drop {
					continue
				}
				_ = ft.Send(ctx, ADU{UnitID: s.cfg.UnitID, PDU: resp, TxnID: adu.TxnID})
				continue
			}
		}
		req, derr := s.pdu.DecodeRequest(adu.PDU)
		if derr != nil {
			me := AsError(derr)
			code := ExceptionCode(ExcIllegalFunction)
			if me != nil && (me.Kind == KindLengthMismatch || me.Kind == KindTruncated) {
				code = ExcIllegalDataValue
			}
			var fc FunctionCode
			if len(adu.PDU) > 0 {
				fc = FunctionCode(adu.PDU[0])
			}
			if adu.UnitID != 0 {
				_ = ft.Send(ctx, ADU{UnitID: s.cfg.UnitID, PDU: EncodeException(fc, code), TxnID: adu.TxnID})
			}
			continue
		}
		respPDU, herr := s.handleRequest(req)
		if adu.UnitID == 0 {
			continue // 广播写不响应
		}
		if herr != nil {
			me := AsError(herr)
			code := ExcServerDeviceFailure
			if me != nil && me.Kind == KindUnsupportedFunction {
				code = ExcIllegalFunction
			} else if errors.Is(herr, errIllegalAddress) {
				code = ExcIllegalDataAddress
			} else if me != nil && me.Kind == KindInvalidArgument {
				code = ExcIllegalDataValue
			}
			_ = ft.Send(ctx, ADU{UnitID: s.cfg.UnitID, PDU: EncodeException(req.FC, code), TxnID: adu.TxnID})
			continue
		}
		_ = ft.Send(ctx, ADU{UnitID: s.cfg.UnitID, PDU: respPDU, TxnID: adu.TxnID})
	}
}

// handleRequest 对单个请求执行内存读写，返回响应 PDU；
// 越界统一报非法数据地址，不支持功能码报非法功能。
func (s *Slave) handleRequest(req Request) ([]byte, error) {
	mem := s.cfg.Memory
	mem.mu.Lock()
	defer mem.mu.Unlock()
	switch req.FC {
	case FCReadCoils, FCReadDiscreteInputs:
		bits, err := readBits(mem, req.FC, req.Address, req.Quantity)
		if err != nil {
			return nil, err
		}
		packed := packBits(bits)
		return s.pdu.EncodeResponse(Response{FC: req.FC, Data: packed})
	case FCReadHoldingRegisters, FCReadInputRegisters:
		regs, err := readRegisters(mem, req.FC, req.Address, req.Quantity)
		if err != nil {
			return nil, err
		}
		data := make([]byte, 2*len(regs))
		for i, v := range regs {
			data[2*i] = byte(v >> 8)
			data[2*i+1] = byte(v)
		}
		return s.pdu.EncodeResponse(Response{FC: req.FC, Data: data})
	case FCWriteSingleCoil:
		if int(req.Address) >= len(mem.Coils) {
			return nil, WrapError(KindInvalidArgument, "coil address out of range", errIllegalAddress)
		}
		v := req.Values[1] == 0xFF
		mem.Coils[req.Address] = v
		return s.pdu.EncodeResponse(Response{FC: req.FC, Address: req.Address, Written: boolCoil(v)})
	case FCWriteSingleRegister:
		if int(req.Address) >= len(mem.HoldingRegisters) {
			return nil, WrapError(KindInvalidArgument, "register address out of range", errIllegalAddress)
		}
		v := uint16(req.Values[0])<<8 | uint16(req.Values[1])
		mem.HoldingRegisters[req.Address] = v
		return s.pdu.EncodeResponse(Response{FC: req.FC, Address: req.Address, Written: v})
	case FCWriteMultipleCoils:
		end := int(req.Address) + int(req.Quantity)
		if end > len(mem.Coils) {
			return nil, WrapError(KindInvalidArgument, "coil range out of bounds", errIllegalAddress)
		}
		for i := 0; i < int(req.Quantity); i++ {
			mem.Coils[int(req.Address)+i] = req.Values[i/8]&(1<<uint(i%8)) != 0
		}
		return s.pdu.EncodeResponse(Response{FC: req.FC, Address: req.Address, Written: req.Quantity})
	case FCWriteMultipleRegisters:
		end := int(req.Address) + int(req.Quantity)
		if end > len(mem.HoldingRegisters) {
			return nil, WrapError(KindInvalidArgument, "register range out of bounds", errIllegalAddress)
		}
		for i := 0; i < int(req.Quantity); i++ {
			mem.HoldingRegisters[int(req.Address)+i] =
				uint16(req.Values[2*i])<<8 | uint16(req.Values[2*i+1])
		}
		return s.pdu.EncodeResponse(Response{FC: req.FC, Address: req.Address, Written: req.Quantity})
	default:
		return nil, NewError(KindUnsupportedFunction, "unsupported function code")
	}
}

func boolCoil(on bool) uint16 {
	if on {
		return 0xFF00
	}
	return 0
}

func readBits(mem *SlaveMemory, fc FunctionCode, addr, qty uint16) ([]bool, error) {
	var src []bool
	switch fc {
	case FCReadCoils:
		src = mem.Coils
	case FCReadDiscreteInputs:
		src = mem.DiscreteInputs
	default:
		return nil, NewError(KindUnsupportedFunction, "not a bit-read function")
	}
	if int(addr)+int(qty) > len(src) {
		return nil, WrapError(KindInvalidArgument, "bit range out of bounds", errIllegalAddress)
	}
	return append([]bool(nil), src[addr:int(addr)+int(qty)]...), nil
}

func readRegisters(mem *SlaveMemory, fc FunctionCode, addr, qty uint16) ([]uint16, error) {
	var src []uint16
	switch fc {
	case FCReadHoldingRegisters:
		src = mem.HoldingRegisters
	case FCReadInputRegisters:
		src = mem.InputRegisters
	default:
		return nil, NewError(KindUnsupportedFunction, "not a register-read function")
	}
	if int(addr)+int(qty) > len(src) {
		return nil, WrapError(KindInvalidArgument, "register range out of bounds", errIllegalAddress)
	}
	return append([]uint16(nil), src[addr:int(addr)+int(qty)]...), nil
}

func packBits(bits []bool) []byte {
	out := make([]byte, (len(bits)+7)/8)
	for i, b := range bits {
		if b {
			out[i/8] |= 1 << uint(i%8)
		}
	}
	return out
}
