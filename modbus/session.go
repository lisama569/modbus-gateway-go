package modbus

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Dialer 建立一条底层连接（支持注入，便于测试与重连）。
type Dialer func(ctx context.Context) (net.Conn, error)

// SessionConfig 配置会话级调度语义。
type SessionConfig struct {
	Transport Transport
	UnitID    byte
	Dial      Dialer
	Timeout   time.Duration // 单次请求等待响应的超时
	MaxRetry  int           // 可重试错误的最大重试次数（不含首次）
}

// Stats 暴露稳定的会话统计（重试次数、重连次数等）。
type Stats struct {
	Requests   int64
	Retries    int64
	Reconnects int64
	Timeouts   int64
}

// Session 表示一条可自愈的主站会话。
//
// 语义保证：
//   - mu 串行化同一连接上的请求：请求-响应严格成对，绝不交叉错配；
//   - TCP 事务标识单调递增并校验响应标识，RTU/ASCII 依赖严格串行定界；
//   - 仅 Timeout / ConnClosed / LengthMismatch 触发“重连+重试”，
//     异常响应、校验失败等确定结论不重试；
//   - 重试在同一 Do 调用内完成，用户层对一次 Do 只看到一次最终结论，
//     写请求不会因库内部行为被重复提交到不同连接之外（同一请求最多
//     在“判定连接已失效”的前提下重发，符合 Modbus 主站惯例）；
//   - 连接惰性建立，失效后下次请求自动重建。
type Session struct {
	cfg   SessionConfig
	codec Codec

	mu     sync.Mutex // 会话级互斥：保证请求顺序与响应不错配
	conn   FrameTransport
	txnSeq uint32
	closed bool

	stats Stats
}

// NewSession 创建会话（不立即拨号，首次请求惰性建连）。
func NewSession(cfg SessionConfig) (*Session, error) {
	codec, err := NewCodec(cfg.Transport)
	if err != nil {
		return nil, err
	}
	if cfg.Dial == nil {
		return nil, NewError(KindInvalidArgument, "Dial is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.MaxRetry < 0 {
		cfg.MaxRetry = 0
	}
	return &Session{cfg: cfg, codec: codec}, nil
}

// Stats 返回会话统计快照。
func (s *Session) Stats() Stats {
	return Stats{
		Requests:   atomic.LoadInt64(&s.stats.Requests),
		Retries:    atomic.LoadInt64(&s.stats.Retries),
		Reconnects: atomic.LoadInt64(&s.stats.Reconnects),
		Timeouts:   atomic.LoadInt64(&s.stats.Timeouts),
	}
}

// Close 关闭会话与底层连接。
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.conn != nil {
		err := s.conn.Close()
		s.conn = nil
		return err
	}
	return nil
}

// nextTxn 分配单调递增的 TCP 事务标识（1..65535 循环）。
func (s *Session) nextTxn() uint16 {
	n := atomic.AddUint32(&s.txnSeq, 1)
	v := uint16(n)
	if v == 0 {
		n = atomic.AddUint32(&s.txnSeq, 1)
		v = uint16(n)
	}
	return v
}

// Do 在会话上发送统一请求并返回统一响应。
func (s *Session) Do(ctx context.Context, req Request) (Response, error) {
	// 会话级互斥在整个“发送+接收+可能重试”期间持有：
	// 同一连接上的请求顺序确定，响应不可能与其他请求错配。
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Response{}, NewError(KindConnClosed, "session closed")
	}
	atomic.AddInt64(&s.stats.Requests, 1)

	// 使用标准 PDU 编码器预编码，以便不支持功能码在发送前即失败。
	std := NewPDU()
	reqPDU, err := std.EncodeRequest(req)
	if err != nil {
		return Response{}, err
	}
	// 单次请求可用 Request.UnitID 覆盖会话默认单元标识。
	unit := s.cfg.UnitID
	if req.UnitID != 0 {
		unit = req.UnitID
	}

	var lastErr error
	maxAttempts := s.cfg.MaxRetry + 1
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			atomic.AddInt64(&s.stats.Retries, 1)
		}
		if err := ctx.Err(); err != nil {
			return Response{}, WrapError(KindTimeout, "context canceled before attempt", err)
		}
		resp, retry, err := s.roundTrip(ctx, req, reqPDU, unit)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retry || attempt == maxAttempts {
			return Response{}, err
		}
		// 重连后再试；连接失效由 roundTrip 已清理。
		s.dropConn()
	}
	return Response{}, lastErr
}

// roundTrip 执行一次收发。返回 retry=true 表示错误可触发重连重试。
func (s *Session) roundTrip(ctx context.Context, req Request, reqPDU []byte, unit byte) (resp Response, retry bool, err error) {
	conn, err := s.ensureConn(ctx)
	if err != nil {
		return Response{}, true, err
	}
	txn := s.nextTxn()
	adu := ADU{UnitID: unit, PDU: reqPDU, TxnID: txn}

	rctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	if err := conn.Send(rctx, adu); err != nil {
		s.dropConn()
		return Response{}, true, classifyIOErr(err)
	}
	raw, err := conn.Recv(rctx)
	if err != nil {
		me := AsError(err)
		if me != nil && (me.Kind == KindTimeout) {
			atomic.AddInt64(&s.stats.Timeouts, 1)
			s.dropConn() // 超时后连接上的帧边界状态不可信，强制重建
			return Response{}, true, me
		}
		if me != nil && (me.Kind == KindConnClosed || me.Kind == KindLengthMismatch) {
			s.dropConn()
			return Response{}, true, me
		}
		// 校验失败、损坏帧等：连接状态同样不可信，重建但不重试（确定结论）。
		s.dropConn()
		return Response{}, false, err
	}
	// 事务/单元标识匹配。
	if s.cfg.Transport == TransportTCP && raw.TxnID != txn {
		s.dropConn()
		return Response{}, false, NewError(KindTransactionMismatch,
			fmt.Sprintf("txn id mismatch: sent %d got %d", txn, raw.TxnID)).
			WithRaw(raw.PDU)
	}
	if raw.UnitID != 0 && raw.UnitID != unit {
		return Response{}, false, NewError(KindTransactionMismatch,
			fmt.Sprintf("unit id mismatch: want %d got %d", unit, raw.UnitID)).
			WithRaw(raw.PDU)
	}
	r, derr := NewPDU().DecodeResponse(raw.PDU, req.FC)
	if derr != nil {
		// 长度不匹配可能因流错位导致：可重试并重建连接。
		if me := AsError(derr); me != nil && me.Kind == KindLengthMismatch {
			s.dropConn()
			return Response{}, true, derr
		}
		return Response{}, false, derr
	}
	return r, false, nil
}

func classifyIOErr(err error) error {
	if AsError(err) != nil {
		return err
	}
	return WrapError(KindConnClosed, "io error", err)
}

// ensureConn 惰性建连。
func (s *Session) ensureConn(ctx context.Context) (FrameTransport, error) {
	if s.conn != nil {
		return s.conn, nil
	}
	dctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	conn, err := s.cfg.Dial(dctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, NewError(KindTimeout, "dial timeout")
		}
		return nil, WrapError(KindConnClosed, "dial failed", err)
	}
	s.conn = NewFrameTransport(conn, s.codec)
	return s.conn, nil
}

// dropConn 关闭并清空当前连接（调用方持锁）。
func (s *Session) dropConn() {
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
		atomic.AddInt64(&s.stats.Reconnects, 1)
	}
}
