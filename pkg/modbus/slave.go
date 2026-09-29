package modbus

import (
	"context"
	"net"
	"sync"
	"time"
)

// InjectRule 是模拟从站的故障注入规则,用于稳定复现各类失败判定。
// 按添加顺序逐条匹配,第一条命中(功能码匹配或为 0)且仍有剩余次数的规则生效。
type InjectRule struct {
	// Function 仅对该功能码生效;为 0 表示对所有功能码生效。
	Function FunctionCode
	// Drop 为 true 时直接丢弃请求不响应(用于触发主站超时)。
	Drop bool
	// ApplyThenDrop 为 true 时,写请求先正常执行(数据区落盘)再丢弃响应,
	// 精确模拟"请求已生效但响应丢失"的不确定场景;仅对写功能码有意义。
	ApplyThenDrop bool
	// Delay 在响应前强制等待。
	Delay time.Duration
	// CorruptChecksum 破坏 RTU CRC / ASCII LRC(TCP 无意义)。
	CorruptChecksum bool
	// CorruptLength 破坏 TCP 长度字段;在 RTU/ASCII 上破坏 CRC/LRC 同样制造帧错误。
	CorruptLength bool
	// CorruptTransaction 破坏 TCP 事务标识。
	CorruptTransaction bool
	// Exception 非 0 时返回该异常码的异常响应(优先级低于 Drop)。
	Exception ExceptionCode
	// NoApply 为 true 时对写请求返回正常回显但不实际改动数据区,
	// 用于复现"响应称成功但值未生效"以及"请求已到达、响应丢失后读回确认"。
	NoApply bool
	// Remaining 为规则剩余生效次数;-1 表示永久生效。
	Remaining int
}

// Slave 是内存实现的本地模拟从站,覆盖四个数据区。
type Slave struct {
	mu       sync.Mutex
	coils    []bool
	discrete []bool
	holding  []uint16
	input    []uint16
	rules    []*InjectRule
	requests int
}

// NewSlave 创建四个数据区各含 size 个地址的模拟从站。
func NewSlave(size int) *Slave {
	if size < 1 {
		size = 65536
	}
	return &Slave{
		coils:    make([]bool, size),
		discrete: make([]bool, size),
		holding:  make([]uint16, size),
		input:    make([]uint16, size),
	}
}

// SetHolding 预置保持寄存器值。
func (s *Slave) SetHolding(addr uint16, value uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if int(addr) < len(s.holding) {
		s.holding[addr] = value
	}
}

// Holding 读取保持寄存器预置值。
func (s *Slave) Holding(addr uint16) (uint16, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if int(addr) >= len(s.holding) {
		return 0, false
	}
	return s.holding[addr], true
}

// SetInput 预置输入寄存器值。
func (s *Slave) SetInput(addr uint16, value uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if int(addr) < len(s.input) {
		s.input[addr] = value
	}
}

// SetCoil 预置线圈值。
func (s *Slave) SetCoil(addr uint16, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if int(addr) < len(s.coils) {
		s.coils[addr] = on
	}
}

// Coil 读取线圈值。
func (s *Slave) Coil(addr uint16) (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if int(addr) >= len(s.coils) {
		return false, false
	}
	return s.coils[addr], true
}

// SetDiscrete 预置离散输入值。
func (s *Slave) SetDiscrete(addr uint16, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if int(addr) < len(s.discrete) {
		s.discrete[addr] = on
	}
}

// AddInject 添加一条故障注入规则。
func (s *Slave) AddInject(rule *InjectRule) {
	if rule == nil {
		return
	}
	s.mu.Lock()
	s.rules = append(s.rules, rule)
	s.mu.Unlock()
}

// ClearInject 清除全部故障注入规则。
func (s *Slave) ClearInject() {
	s.mu.Lock()
	s.rules = nil
	s.mu.Unlock()
}

// RequestCount 返回从站已收到的请求数。
func (s *Slave) RequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// matchRule 找到并消耗(递减剩余次数)一条命中的注入规则。
func (s *Slave) matchRule(fc FunctionCode) *InjectRule {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rules {
		if r.Function != 0 && r.Function != fc {
			continue
		}
		if r.Remaining == 0 {
			continue
		}
		if r.Remaining > 0 {
			r.Remaining--
		}
		return r
	}
	return nil
}

// handle 处理一帧请求,返回要写回的响应帧;返回 nil 表示不响应(丢弃)。
// out 非 nil 时包含传输层破坏指令(由 server 侧据此篡改字节)。
func (s *Slave) handle(f *Frame) (resp *Frame, rule *InjectRule) {
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()

	req, derr := DecodeRequestPDU(f.PDU)
	if derr != nil {
		// 畸形请求:功能码不可识别时以 0 回应非法数据值异常。
		fc := FunctionCode(0)
		if len(f.PDU) > 0 {
			fc = FunctionCode(f.PDU[0])
		}
		return &Frame{Unit: f.Unit, PDU: ExceptionResponsePDU(fc, ExcIllegalDataValue)}, nil
	}

	rule = s.matchRule(req.Function)
	if rule != nil {
		if rule.Delay > 0 {
			time.Sleep(rule.Delay)
		}
		if rule.Drop {
			// 先落数据再静默:模拟写请求已被设备执行、但响应在链路中丢失。
			if rule.ApplyThenDrop && !isReadFC(req.Function) {
				_ = s.serveData(req, nil)
			}
			return nil, rule
		}
		if rule.Exception != 0 {
			return &Frame{Unit: f.Unit, TransactionID: f.TransactionID,
				PDU: ExceptionResponsePDU(req.Function, rule.Exception)}, rule
		}
	}

	pdu := s.serveData(req, rule)
	return &Frame{Unit: f.Unit, TransactionID: f.TransactionID, PDU: pdu}, rule
}

// serveData 在锁保护下读写数据区并构造正常/异常响应 PDU。
func (s *Slave) serveData(req *Request, rule *InjectRule) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	size := len(s.holding)
	checkRange := func(addr, qty uint16) bool {
		return int(addr)+int(qty) <= size
	}

	// NoApply:写请求返回正常回显但不改动数据区,模拟"响应成功但未生效",
	// 供主站读回确认与"可能已生效"语义测试。
	if rule != nil && rule.NoApply && !isReadFC(req.Function) {
		r := &Response{Function: req.Function, Address: req.Address,
			Value: req.Value, Quantity: req.Quantity}
		p, _ := r.EncodePDU()
		return p
	}

	switch req.Function {
	case FCReadCoils:
		if !checkRange(req.Address, req.Quantity) {
			return ExceptionResponsePDU(req.Function, ExcIllegalDataAddress)
		}
		bits := s.coils[req.Address : req.Address+req.Quantity]
		r := &Response{Function: req.Function, Bits: bits}
		p, err := r.EncodePDU()
		if err != nil {
			return ExceptionResponsePDU(req.Function, ExcIllegalDataValue)
		}
		return p

	case FCReadDiscreteInputs:
		if !checkRange(req.Address, req.Quantity) {
			return ExceptionResponsePDU(req.Function, ExcIllegalDataAddress)
		}
		bits := s.discrete[req.Address : req.Address+req.Quantity]
		r := &Response{Function: req.Function, Bits: bits}
		p, err := r.EncodePDU()
		if err != nil {
			return ExceptionResponsePDU(req.Function, ExcIllegalDataValue)
		}
		return p

	case FCReadHoldingRegisters:
		if !checkRange(req.Address, req.Quantity) {
			return ExceptionResponsePDU(req.Function, ExcIllegalDataAddress)
		}
		regs := append([]uint16(nil), s.holding[req.Address:req.Address+req.Quantity]...)
		r := &Response{Function: req.Function, Registers: regs}
		p, err := r.EncodePDU()
		if err != nil {
			return ExceptionResponsePDU(req.Function, ExcIllegalDataValue)
		}
		return p

	case FCReadInputRegisters:
		if !checkRange(req.Address, req.Quantity) {
			return ExceptionResponsePDU(req.Function, ExcIllegalDataAddress)
		}
		regs := append([]uint16(nil), s.input[req.Address:req.Address+req.Quantity]...)
		r := &Response{Function: req.Function, Registers: regs}
		p, err := r.EncodePDU()
		if err != nil {
			return ExceptionResponsePDU(req.Function, ExcIllegalDataValue)
		}
		return p

	case FCWriteSingleCoil:
		if int(req.Address) >= size {
			return ExceptionResponsePDU(req.Function, ExcIllegalDataAddress)
		}
		s.coils[req.Address] = req.Value == 0xFF00
		r := &Response{Function: req.Function, Address: req.Address, Value: req.Value}
		p, _ := r.EncodePDU()
		return p

	case FCWriteSingleRegister:
		if int(req.Address) >= size {
			return ExceptionResponsePDU(req.Function, ExcIllegalDataAddress)
		}
		s.holding[req.Address] = req.Value
		r := &Response{Function: req.Function, Address: req.Address, Value: req.Value}
		p, _ := r.EncodePDU()
		return p

	case FCWriteMultipleCoils:
		if !checkRange(req.Address, req.Quantity) {
			return ExceptionResponsePDU(req.Function, ExcIllegalDataAddress)
		}
		for i := uint16(0); i < req.Quantity; i++ {
			s.coils[req.Address+uint16(i)] = req.Bytes[i/8]&(1<<uint(i%8)) != 0
		}
		r := &Response{Function: req.Function, Address: req.Address, Quantity: req.Quantity}
		p, _ := r.EncodePDU()
		return p

	case FCWriteMultipleRegisters:
		if !checkRange(req.Address, req.Quantity) {
			return ExceptionResponsePDU(req.Function, ExcIllegalDataAddress)
		}
		for i := uint16(0); i < req.Quantity; i++ {
			s.holding[req.Address+uint16(i)] = Uint16(req.Bytes[i*2:])
		}
		r := &Response{Function: req.Function, Address: req.Address, Quantity: req.Quantity}
		p, _ := r.EncodePDU()
		return p
	}
	return ExceptionResponsePDU(req.Function, ExcIllegalFunction)
}

// Server 是承载在本地回环 TCP 监听器上的模拟从站服务端。
// RTU/ASCII 的分帧与校验语义由 Codec 保证,传输层仅复用本地流,
// 因此三种形态对上层呈现完全一致的请求/响应/失败模型。
type Server struct {
	transport Transport
	listener  net.Listener
	slave     *Slave
	codec     Codec

	wg     sync.WaitGroup
	connMu sync.Mutex
	conns  map[net.Conn]struct{}
	closed chan struct{}
}

// ServeLocal 在 127.0.0.1 随机端口上启动指定传输形态的本地模拟从站。
func ServeLocal(transport Transport, slave *Slave) (*Server, error) {
	codec, err := NewCodec(transport)
	if err != nil {
		return nil, err
	}
	if slave == nil {
		slave = NewSlave(65536)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := newServer(transport, ln, slave, codec)
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Addr 返回本地监听地址(供 Client 拨号)。
func (s *Server) Addr() string { return s.listener.Addr().String() }

func newServer(transport Transport, ln net.Listener, slave *Slave, codec Codec) *Server {
	return &Server{
		transport: transport,
		listener:  ln,
		slave:     slave,
		codec:     codec,
		conns:     make(map[net.Conn]struct{}),
		closed:    make(chan struct{}),
	}
}

// ServeLocalAt 与 ServeLocal 相同,但绑定到指定本地地址(用于连接重建测试:
// 服务端关闭后在同一地址重启)。
func ServeLocalAt(transport Transport, slave *Slave, addr string) (*Server, error) {
	codec, err := NewCodec(transport)
	if err != nil {
		return nil, err
	}
	if slave == nil {
		slave = NewSlave(65536)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := newServer(transport, ln, slave, codec)
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Close 关闭监听器与全部活动连接,并等待所有处理结束。
func (s *Server) Close() error {
	select {
	case <-s.closed:
		return nil
	default:
		close(s.closed)
	}
	err := s.listener.Close()
	s.connMu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.conns = make(map[net.Conn]struct{})
	s.connMu.Unlock()
	s.wg.Wait()
	return err
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-s.closed
		cancel()
	}()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.connMu.Lock()
		select {
		case <-s.closed:
			s.connMu.Unlock()
			_ = conn.Close()
			return
		default:
			s.conns[conn] = struct{}{}
		}
		s.connMu.Unlock()
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer c.Close()
			defer func() {
				s.connMu.Lock()
				delete(s.conns, c)
				s.connMu.Unlock()
			}()
			s.serveConn(ctx, c)
		}(conn)
	}
}

// serveConn 在单连接上按帧串行服务(请求-响应严格交替)。
func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	for {
		f, err := s.codec.Decode(conn, DirRequest)
		if err != nil {
			// 客户端关闭/超时/帧错误均结束本连接,不再尝试"跳帧",
			// 保证一帧错误即令连接失效,语义稳定。
			return
		}
		resp, rule := s.slave.handle(f)
		if resp == nil {
			// Drop: 不回任何字节。此后该连接进入"排空"状态:继续读取并丢弃
			// 后续帧但绝不响应,直到主站超时关闭连接(EOF)。这样主站侧稳定得到
			// 超时判定,且重连前残留在本连接上的帧也不会产生迟到响应混入新事务。
			s.drainUntilClosed(conn)
			return
		}
		out, err := s.codec.Encode(resp)
		if err != nil {
			return
		}
		out = s.applyCorruption(out, rule)
		_, _ = conn.Write(out)
	}
}

// drainUntilClosed 在 Drop 之后排空连接:读到帧也不响应,直到连接关闭。
func (s *Server) drainUntilClosed(conn net.Conn) {
	for {
		if _, err := s.codec.Decode(conn, DirRequest); err != nil {
			return
		}
	}
}

// applyCorruption 按注入规则破坏响应字节,用于复现校验/长度/事务标识错误。
func (s *Server) applyCorruption(raw []byte, rule *InjectRule) []byte {
	if rule == nil {
		return raw
	}
	out := append([]byte(nil), raw...)
	switch s.transport {
	case TransportTCP:
		// MBAP: [0:2] txn [2:4] proto [4:6] length [6] unit [7:] pdu
		if rule.CorruptLength && len(out) >= 6 {
			// 长度字段 +3,与实际载荷不符;末尾不足字节会让主站读截断。
			out[4] += 3
		}
		if rule.CorruptTransaction && len(out) >= 2 {
			out[0] ^= 0xFF
			out[1] ^= 0xFF
		}
		if (rule.CorruptChecksum || (rule.CorruptLength && len(out) < 7)) && len(out) > 7 {
			out[len(out)-1] ^= 0xFF
		}
	case TransportRTU:
		if rule.CorruptChecksum || rule.CorruptLength {
			out[len(out)-1] ^= 0xFF
		}
	case TransportASCII:
		if rule.CorruptChecksum || rule.CorruptLength {
			// 破坏 LRC 两字符(位于 CRLF 之前)。
			if len(out) >= 4 {
				out[len(out)-3] = tweakHex(out[len(out)-3])
			}
		}
	}
	return out
}

func tweakHex(c byte) byte {
	if c == '0' {
		return '1'
	}
	return '0'
}
