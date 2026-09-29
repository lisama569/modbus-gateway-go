package modbus

// maxWriteConfirmRounds 是首次写入不确定后允许的"写入+确认"恢复轮数上限。
// 加上首次尝试,物理写入最多 maxWriteConfirmRounds+1 次;确认读次数另计。
const maxWriteConfirmRounds = 2

// writeOutcome 是一次安全写入的最终结论。
type writeOutcome struct {
	attempts int
	// uncertain 为 true 表示"请求可能已生效但无法确认最终状态";
	// err == nil 即确定成功;err != nil 且 uncertain == false 即确定失败(对端明确拒绝)。
	uncertain bool
	err       error
}

// SafeWriteCoils 安全写入一段连续线圈(整段一把会话锁,读回确认与写入不被插队):
//
//   - 对端明确拒绝(异常响应/非法参数等):立即返回该错误,绝不重发;
//   - 超时/连接失效/帧错误等不确定错误:在新连接(新事务)上重发,
//     并通过 FC01 读回目标地址区间与期望值逐位比对确认最终状态;
//   - 读回一致 => 成功(响应虽丢但值确实生效);读回不一致且确认读可靠
//     => 再走一轮"写入+确认";确认本身也失败或轮数用尽 => 返回 KindUncertain,
//     由上层按"可能已生效"处理,绝不盲报成功或失败;
//   - 旧连接在每轮恢复前关闭,迟到响应随连接丢弃,不会混入后续事务的结果。
func (c *Client) SafeWriteCoils(addr uint16, bits []bool) (attempts int, err error) {
	o := c.safeWrite(&Request{
		Function: FCWriteMultipleCoils, Address: addr,
		Quantity: uint16(len(bits)), Bytes: packCoils(bits),
	}, len(bits), func() (match bool, ok bool, cerr error) {
		got, rerr := c.readCoilsLocked(addr, uint16(len(bits)))
		if rerr != nil {
			return false, false, rerr
		}
		for i := range bits {
			if got[i] != bits[i] {
				return false, true, nil
			}
		}
		return true, true, nil
	})
	if o.err != nil {
		return o.attempts, o.err
	}
	return o.attempts, nil
}

// SafeWriteRegisters 安全写入一段连续保持寄存器,语义同 SafeWriteCoils,确认读用 FC03。
func (c *Client) SafeWriteRegisters(addr uint16, regs []uint16) (attempts int, err error) {
	raw := make([]byte, len(regs)*2)
	for i, v := range regs {
		putUint16(raw[i*2:], v)
	}
	o := c.safeWrite(&Request{
		Function: FCWriteMultipleRegisters, Address: addr,
		Quantity: uint16(len(regs)), Bytes: raw,
	}, len(regs)*2, func() (match bool, ok bool, cerr error) {
		got, rerr := c.readRegistersLocked(addr, uint16(len(regs)))
		if rerr != nil {
			return false, false, rerr
		}
		for i := range regs {
			if got[i] != regs[i] {
				return false, true, nil
			}
		}
		return true, true, nil
	})
	if o.err != nil {
		return o.attempts, o.err
	}
	return o.attempts, nil
}

// packCoils 将位数组打包为 FC0F 载荷字节。
func packCoils(bits []bool) []byte {
	raw := make([]byte, (len(bits)+7)/8)
	for i, b := range bits {
		if b {
			raw[i/8] |= 1 << uint(i%8)
		}
	}
	return raw
}

// safeWrite 执行"写入 + 不确定恢复读回确认"状态机,调用方外部无需持锁;
// 整个状态机持有会话锁,因此并发写入不会串台,写后确认读到的必是本写的目标区间。
//
// expectLen 用于参数本地校验;confirm 完成一次读回并报告:
//
//	match=true  设备当前状态与期望一致
//	ok=true     读回本身成功(match 才有意义);ok=false 表示确认读失败
func (c *Client) safeWrite(req *Request, expectLen int, confirm func() (match, ok bool, err error)) writeOutcome {
	if expectLen == 0 {
		return writeOutcome{err: &Error{Kind: KindAddress, Msg: "safe write: empty payload"}}
	}
	pdu, err := req.EncodePDU()
	if err != nil {
		return writeOutcome{err: err} // 请求模型非法,不重试
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return writeOutcome{err: &Error{Kind: KindClosed, Msg: "client closed"}}
	}

	// 首次尝试:成功 => 直接确认;明确拒绝 => 直接失败;不确定 => 进入恢复。
	perr := c.physicalWriteLocked(pdu, req)
	attempts := 1
	if perr == nil {
		return writeOutcome{attempts: attempts}
	}
	if !retryable(perr) {
		return writeOutcome{attempts: attempts, err: perr}
	}

	// 恢复循环:关闭旧连接(丢弃迟到响应)-> 新连接新事务重发 -> 读回确认。
	var lastErr = perr
	for round := 0; round < maxWriteConfirmRounds; round++ {
		c.closeInvalid()
		if derr := c.connect(); derr != nil {
			lastErr = derr
			continue
		}
		perr = c.physicalWriteLocked(pdu, req)
		attempts++
		if perr == nil {
			// 重发得到明确成功应答。旧连接上的写是否已生效不再重要:
			// 本次应答本身承诺目标值已写入。
			return writeOutcome{attempts: attempts}
		}
		if !retryable(perr) {
			// 重发后对端明确拒绝:此前的不确定尝试结果不可知,但设备当前明确拒绝
			// 相同写入。按"可能已生效"上报,由上层读回决策;不继续盲发。
			return writeOutcome{attempts: attempts, uncertain: true,
				err: &Error{Kind: KindUncertain,
					Msg:   "write earlier outcome uncertain, device now rejects: " + perr.Error(),
					Cause: perr}}
		}
		lastErr = perr

		// 写不确定已令当前连接失效;读回必须发生在新连接上,
		// 旧连接的迟到响应随关闭丢弃,不会混入读回结果。
		c.closeInvalid()
		if derr := c.connect(); derr != nil {
			lastErr = &Error{Kind: KindUncertain,
				Msg:   "write outcome uncertain; reconnect for read-back failed: " + derr.Error(),
				Cause: derr}
			continue
		}
		// 读回确认当前真实状态。读回一致 => 之前的写已生效,以已发生的尝试次数成功返回;
		// 读回不一致 => 写未生效,continue 进入下一轮重发。
		match, ok, cerr := confirm()
		if ok && match {
			return writeOutcome{attempts: attempts}
		}
		if ok && !match {
			// 读回可靠但值不一致:写未生效,下一轮重发。
			lastErr = &Error{Kind: KindUncertain,
				Msg: "read-back mismatch: write did not take effect, will retry"}
			continue
		}
		// 确认读本身失败:若为明确拒绝(地址类),状态无法经读回判定。
		lastErr = &Error{Kind: KindUncertain,
			Msg: "write outcome uncertain; read-back failed: " + safeErrText(cerr)}
		if cerr != nil && !retryable(cerr) {
			// 读回被明确拒绝,继续重发写也无法获得确认,直接上报不确定。
			return writeOutcome{attempts: attempts, uncertain: true, err: lastErr}
		}
	}
	return writeOutcome{attempts: attempts, uncertain: true, err: lastErr}
}

// physicalWriteLocked 在当前连接上执行一次物理写(新事务标识),调用方持会话锁。
func (c *Client) physicalWriteLocked(pdu []byte, req *Request) error {
	c.txnSeq++
	txn := c.txnSeq
	frame := &Frame{Unit: c.cfg.Unit, PDU: pdu, TransactionID: txn}
	raw, encErr := c.codec.Encode(frame)
	if encErr != nil {
		return encErr
	}
	c.logf("tx safe-write transport=%s unit=%d fc=0x%02X txn=%d raw=%x",
		c.codec.Name(), frame.Unit, byte(req.Function), txn, raw)
	_, perr := c.roundtrip(frame, req.Function)
	if perr != nil {
		c.logf("rx safe-write normalized=%s kind=%d", perr.Error(), int(KindOf(perr)))
	}
	return perr
}

// readCoilsLocked 在会话锁内执行 FC01 并截取指定位数。
func (c *Client) readCoilsLocked(addr, qty uint16) ([]bool, error) {
	r, err := c.doLocked(&Request{Function: FCReadCoils, Address: addr, Quantity: qty})
	if err != nil {
		c.closeInvalid()
		return nil, err
	}
	return r.Bits[:qty], nil
}

// readRegistersLocked 在会话锁内执行 FC03。
func (c *Client) readRegistersLocked(addr, qty uint16) ([]uint16, error) {
	r, err := c.doLocked(&Request{Function: FCReadHoldingRegisters,
		Address: addr, Quantity: qty})
	if err != nil {
		c.closeInvalid()
		return nil, err
	}
	return r.Registers, nil
}

// doLocked 在已持有会话锁的前提下执行一次请求-响应(不重试,不重连;
// 连接失效由调用方在恢复循环中处理)。
func (c *Client) doLocked(req *Request) (*Response, error) {
	pdu, err := req.EncodePDU()
	if err != nil {
		return nil, err
	}
	if c.conn == nil {
		if derr := c.connect(); derr != nil {
			return nil, derr
		}
	}
	c.txnSeq++
	txn := c.txnSeq
	frame := &Frame{Unit: c.cfg.Unit, PDU: pdu, TransactionID: txn}
	if _, encErr := c.codec.Encode(frame); encErr != nil {
		return nil, encErr
	}
	return c.roundtrip(frame, req.Function)
}

func safeErrText(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}
