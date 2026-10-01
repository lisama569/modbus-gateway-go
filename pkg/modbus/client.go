package modbus

import (
	"net"
	"sync"
	"time"
)

// 默认会话参数。
const (
	defaultTimeout        = 2 * time.Second
	defaultConnectTimeout = 2 * time.Second
	defaultRetry          = 0
)

// Logger 用于输出原始报文与归一化判定结果(测试中注入 testing 日志)。
type Logger interface {
	Printf(format string, args ...any)
}

// ClientConfig 是会话级客户端配置。
type ClientConfig struct {
	Transport      Transport
	Addr           string
	Unit           byte
	Timeout        time.Duration // 单次请求响应超时
	Retry          int           // 失败后的额外尝试次数(不含首次)
	ConnectTimeout time.Duration // 单次拨号超时
	// Logger 非 nil 时打印每次收发的原始报文与判定结果。
	Logger Logger
}

func (c ClientConfig) withDefaults() ClientConfig {
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = defaultConnectTimeout
	}
	if c.Retry < 0 {
		c.Retry = 0
	}
	return c
}

// Client 是会话级 Modbus 客户端。
//
// 语义保证:
//   - 会话内单把串行锁(mu),同一连接上的请求严格 FIFO,请求与响应一一对应,
//     绝不会交叉错配。
//   - 每次物理请求占用一个新的 TCP 事务标识;RTU/ASCII 无事务标识,依靠串行锁配对。
//   - 超时、校验失败、长度不符、事务标识不符、帧截断等均使当前连接失效;
//     重试前先重建连接,旧连接上的迟到响应随连接关闭被丢弃,绝不混入新请求结果。
//   - 可重试错误才重试(超时、连接失效、帧截断、校验、长度与事务标识错误);
//     异常响应、地址非法、不支持功能码属"明确答复",不重试。
//   - 重试是"物理重发":每次尝试都是全新连接上的新事务,语义上同一逻辑请求
//     不会在同一连接上重复提交,结果也不会混淆。
type Client struct {
	cfg   ClientConfig
	codec Codec

	mu     sync.Mutex
	conn   net.Conn
	txnSeq uint16
	closed bool

	// batchMu 是批次级互斥锁,与单请求串行锁 mu 分离:
	// 一个批量写回的完整"下发—读回确认—重发"循环会全程持有它,
	// 期间对 Do 的每次调用仍按单请求路径加 mu,因此不能直接复用 mu。
	batchMu sync.Mutex
}

// LockBatch / UnlockBatch 实现 mapping.BatchGuard:同一客户端上的批量写回
// 严格串行。锁内包含该批次的全部物理操作(含读回确认与恢复重发),
// 任何其他批次都不能插到某批次的下发与确认之间。
func (c *Client) LockBatch()   { c.batchMu.Lock() }
func (c *Client) UnlockBatch() { c.batchMu.Unlock() }

// Dial 创建客户端并建立首条连接。
func Dial(cfg ClientConfig) (*Client, error) {
	cfg = cfg.withDefaults()
	codec, err := NewCodec(cfg.Transport)
	if err != nil {
		return nil, err
	}
	c := &Client{cfg: cfg, codec: codec}
	if err := c.connect(); err != nil {
		return nil, err
	}
	return c, nil
}

// connect 建立底层连接(调用方须持有/不依赖旧连接)。
func (c *Client) connect() error {
	d := net.Dialer{Timeout: c.cfg.ConnectTimeout}
	conn, err := d.Dial("tcp", c.cfg.Addr)
	if err != nil {
		return &Error{Kind: KindClosed, Msg: "dial " + c.cfg.Addr + " failed: " + err.Error(), Cause: err}
	}
	c.conn = conn
	return nil
}

// closeInvalid 关闭并丢弃当前连接,确保旧连接上的迟到响应不会混入新会话。
func (c *Client) closeInvalid() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// Close 关闭客户端与底层连接。
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

// retryable 判断该错误是否触发重试。
func retryable(err error) bool {
	switch KindOf(err) {
	case KindTimeout, KindClosed, KindTruncated,
		KindChecksum, KindLengthMismatch, KindTransactionMismatch:
		return true
	}
	return false
}

// Do 发送一个统一请求并返回统一响应(串行、可超时、可重试)。
func (c *Client) Do(req *Request) (*Response, error) {
	return c.doAttempts(req, c.cfg.Retry, nil)
}

// doAttempts 执行请求并在可重试错误时重建连接重试,至多尝试 retry+1 次。
// attemptHook 在每次物理尝试后被调用(仅测试用),参数为尝试序号(0 起)。
func (c *Client) doAttempts(req *Request, retry int, attemptHook func(attempt int)) (*Response, error) {
	pdu, err := req.EncodePDU()
	if err != nil {
		return nil, err // 请求模型本身非法,不重试
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, &Error{Kind: KindClosed, Msg: "client closed"}
	}

	var lastErr error
	maxAttempts := retry + 1
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			c.closeInvalid()
			if derr := c.connect(); derr != nil {
				lastErr = derr
				continue
			}
		}
		if c.conn == nil {
			if derr := c.connect(); derr != nil {
				lastErr = derr
				continue
			}
		}

		c.txnSeq++
		txn := c.txnSeq
		frame := &Frame{Unit: c.cfg.Unit, PDU: pdu, TransactionID: txn}
		raw, encErr := c.codec.Encode(frame)
		if encErr != nil {
			return nil, encErr
		}
		c.logf("tx attempt=%d transport=%s unit=%d fc=0x%02X txn=%d raw=%x",
			attempt, c.codec.Name(), frame.Unit, byte(req.Function), txn, raw)

		resp, perr := c.roundtrip(frame, req.Function)
		if attemptHook != nil {
			attemptHook(attempt)
		}
		if perr == nil {
			c.logf("rx attempt=%d normalized=OK fc=0x%02X registers=%d bits=%d",
				attempt, byte(req.Function), len(resp.Registers), len(resp.Bits))
			return resp, nil
		}
		c.logf("rx attempt=%d normalized=%s kind=%d raw=%x",
			attempt, perr.Error(), int(KindOf(perr)), rawOf(perr))
		lastErr = perr
		if !retryable(perr) {
			// 明确答复(异常响应/地址/不支持功能):不重试,直接返回。
			// 连接仍保留(异常响应本身是完好的帧)。
			return nil, perr
		}
		// 可重试错误:当前连接已不可信,循环内下轮重建。
		c.closeInvalid()
	}
	return nil, lastErr
}

// roundtrip 执行单次物理请求-响应,调用方已持有串行锁。
func (c *Client) roundtrip(frame *Frame, fc FunctionCode) (*Response, error) {
	deadline := time.Now().Add(c.cfg.Timeout)
	if err := c.conn.SetDeadline(deadline); err != nil {
		c.closeInvalid()
		return nil, &Error{Kind: KindClosed, Msg: "set deadline failed: " + err.Error(), Cause: err}
	}
	if _, err := c.conn.Write(frame.Raw); err != nil {
		c.closeInvalid()
		return nil, mapWriteErr(err)
	}
	respFrame, err := c.codec.Decode(c.conn, DirResponse)
	if err != nil {
		// 截断/校验/长度错误与超时一样,都令连接失效。
		c.closeInvalid()
		return nil, err
	}
	// 单元标识核对(RTU/ASCII 为从站地址,TCP 为 Unit ID)。
	if respFrame.Unit != frame.Unit {
		c.closeInvalid()
		return nil, &Error{
			Kind: KindTransactionMismatch,
			Msg: "unit identifier mismatch: got " + itoaI(int(respFrame.Unit)) +
				", want " + itoaI(int(frame.Unit)),
			Raw: respFrame.Raw,
		}
	}
	// TCP 事务标识必须与本请求一一对应。
	if c.cfg.Transport == TransportTCP && respFrame.TransactionID != frame.TransactionID {
		c.closeInvalid()
		return nil, &Error{
			Kind: KindTransactionMismatch,
			Msg: "tcp transaction id mismatch: got " + itoaI(int(respFrame.TransactionID)) +
				", want " + itoaI(int(frame.TransactionID)),
			Raw: respFrame.Raw,
		}
	}
	return DecodeResponsePDU(fc, respFrame.PDU)
}

func (c *Client) logf(format string, args ...any) {
	if c.cfg.Logger != nil {
		c.cfg.Logger.Printf(format, args...)
	}
}

func rawOf(err error) []byte {
	if e, ok := err.(*Error); ok {
		return e.Raw
	}
	return nil
}

func mapWriteErr(err error) *Error {
	var ne net.Error
	if asNetErr(err, &ne) && ne.Timeout() {
		return &Error{Kind: KindTimeout, Msg: "write timeout: " + err.Error(), Cause: err}
	}
	return &Error{Kind: KindClosed, Msg: "write failed: " + err.Error(), Cause: err}
}

func asNetErr(err error, target *net.Error) bool {
	if e, ok := err.(net.Error); ok {
		*target = e
		return true
	}
	return false
}

func itoaI(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// --- 便捷方法 ---

func (c *Client) ReadCoils(addr, qty uint16) ([]bool, error) {
	r, err := c.Do(&Request{Function: FCReadCoils, Address: addr, Quantity: qty})
	if err != nil {
		return nil, err
	}
	return r.Bits[:qty], nil
}

func (c *Client) ReadDiscreteInputs(addr, qty uint16) ([]bool, error) {
	r, err := c.Do(&Request{Function: FCReadDiscreteInputs, Address: addr, Quantity: qty})
	if err != nil {
		return nil, err
	}
	return r.Bits[:qty], nil
}

func (c *Client) ReadHoldingRegisters(addr, qty uint16) ([]uint16, error) {
	r, err := c.Do(&Request{Function: FCReadHoldingRegisters, Address: addr, Quantity: qty})
	if err != nil {
		return nil, err
	}
	return r.Registers, nil
}

func (c *Client) ReadInputRegisters(addr, qty uint16) ([]uint16, error) {
	r, err := c.Do(&Request{Function: FCReadInputRegisters, Address: addr, Quantity: qty})
	if err != nil {
		return nil, err
	}
	return r.Registers, nil
}

func (c *Client) WriteSingleCoil(addr uint16, on bool) error {
	v := uint16(0)
	if on {
		v = 0xFF00
	}
	_, err := c.Do(&Request{Function: FCWriteSingleCoil, Address: addr, Value: v})
	return err
}

func (c *Client) WriteSingleRegister(addr, value uint16) error {
	_, err := c.Do(&Request{Function: FCWriteSingleRegister, Address: addr, Value: value})
	return err
}

func (c *Client) WriteMultipleCoils(addr uint16, bits []bool) error {
	qty := uint16(len(bits))
	if qty == 0 {
		return &Error{Kind: KindAddress, Msg: "write multiple coils: empty payload"}
	}
	raw := make([]byte, (qty+7)/8)
	for i, b := range bits {
		if b {
			raw[i/8] |= 1 << uint(i%8)
		}
	}
	_, err := c.Do(&Request{Function: FCWriteMultipleCoils, Address: addr,
		Quantity: qty, Bytes: raw})
	return err
}

func (c *Client) WriteMultipleRegisters(addr uint16, regs []uint16) error {
	qty := uint16(len(regs))
	if qty == 0 {
		return &Error{Kind: KindAddress, Msg: "write multiple registers: empty payload"}
	}
	raw := make([]byte, qty*2)
	for i, v := range regs {
		putUint16(raw[i*2:], v)
	}
	_, err := c.Do(&Request{Function: FCWriteMultipleRegisters, Address: addr,
		Quantity: qty, Bytes: raw})
	return err
}
