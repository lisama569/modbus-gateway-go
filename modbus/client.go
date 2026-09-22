package modbus

import "context"

// Client 是在 Session 之上的高层读写 API。
type Client struct {
	sess *Session
}

// NewClient 包装已有会话。
func NewClient(sess *Session) *Client { return &Client{sess: sess} }

// ReadBits 读取线圈/离散输入，返回每个点位的布尔值。
func (c *Client) ReadBits(ctx context.Context, region Region, addr, quantity uint16) ([]bool, error) {
	if !validBitRegion(region) {
		return nil, NewError(KindInvalidArgument, "region is not a bit area: "+region.String())
	}
	if quantity == 0 || int(addr)+int(quantity) > 0x10000 {
		return nil, NewError(KindInvalidArgument, "illegal bit read range")
	}
	// 超过单次 PDU 容量时按确定边界拆分，顺序提交。
	var out []bool
	for remaining, cur := quantity, addr; remaining > 0; {
		n := remaining
		if n > MaxBitsPerReadPDU {
			n = MaxBitsPerReadPDU
		}
		resp, err := c.sess.Do(ctx, Request{FC: region.ReadFC(), Address: cur, Quantity: n})
		if err != nil {
			return nil, err
		}
		bits, err := unpackBits(resp.Data, n)
		if err != nil {
			return nil, err
		}
		out = append(out, bits...)
		cur += n
		remaining -= n
	}
	return out, nil
}

// ReadRegisters 读取寄存器区，返回原始 16 位大端字。
func (c *Client) ReadRegisters(ctx context.Context, region Region, addr, quantity uint16) ([]uint16, error) {
	if !region.IsRegister() {
		return nil, NewError(KindInvalidArgument, "region is not a register area: "+region.String())
	}
	if quantity == 0 || int(addr)+int(quantity) > 0x10000 {
		return nil, NewError(KindInvalidArgument, "illegal register read range")
	}
	var out []uint16
	remaining, cur := quantity, addr
	for remaining > 0 {
		n := remaining
		if n > MaxRegistersPerReadPDU {
			n = MaxRegistersPerReadPDU
		}
		resp, err := c.sess.Do(ctx, Request{FC: region.ReadFC(), Address: cur, Quantity: n})
		if err != nil {
			return nil, err
		}
		if len(resp.Data) != int(2*n) {
			return nil, NewError(KindLengthMismatch, "register data length disagrees with quantity")
		}
		for i := 0; i < int(n); i++ {
			out = append(out, uint16(resp.Data[2*i])<<8|uint16(resp.Data[2*i+1]))
		}
		cur += n
		remaining -= n
	}
	return out, nil
}

// WriteSingleCoil 写单个线圈（on=true 编码为 0xFF00）。
func (c *Client) WriteSingleCoil(ctx context.Context, addr uint16, on bool) error {
	v := []byte{0x00, 0x00}
	if on {
		v[0] = 0xFF
	}
	_, err := c.sess.Do(ctx, Request{FC: FCWriteSingleCoil, Address: addr, Values: v})
	return err
}

// WriteSingleRegister 写单个保持寄存器。
func (c *Client) WriteSingleRegister(ctx context.Context, addr, value uint16) error {
	_, err := c.sess.Do(ctx, Request{
		FC:      FCWriteSingleRegister,
		Address: addr,
		Values:  []byte{byte(value >> 8), byte(value)},
	})
	return err
}

// WriteMultipleCoils 批量写线圈，按 PDU 容量确定拆分（每片不超过 1968 点）。
func (c *Client) WriteMultipleCoils(ctx context.Context, addr uint16, values []bool) error {
	if len(values) == 0 || int(addr)+len(values) > 0x10000 {
		return NewError(KindInvalidArgument, "illegal coil write range")
	}
	for off := 0; off < len(values); {
		n := len(values) - off
		if n > MaxCoilsPerWritePDU {
			n = MaxCoilsPerWritePDU
		}
		payload := packBits(values[off : off+n])
		if _, err := c.sess.Do(ctx, Request{
			FC: FCWriteMultipleCoils, Address: addr + uint16(off),
			Quantity: uint16(n), Values: payload,
		}); err != nil {
			return err
		}
		off += n
	}
	return nil
}

// WriteMultipleRegisters 批量写保持寄存器，按 PDU 容量确定拆分（每片不超过 123 个）。
func (c *Client) WriteMultipleRegisters(ctx context.Context, addr uint16, values []uint16) error {
	if len(values) == 0 || int(addr)+len(values) > 0x10000 {
		return NewError(KindInvalidArgument, "illegal register write range")
	}
	for off := 0; off < len(values); {
		n := len(values) - off
		if n > MaxRegistersPerWritePDU {
			n = MaxRegistersPerWritePDU
		}
		payload := make([]byte, 2*n)
		for i := 0; i < n; i++ {
			v := values[off+i]
			payload[2*i] = byte(v >> 8)
			payload[2*i+1] = byte(v)
		}
		if _, err := c.sess.Do(ctx, Request{
			FC: FCWriteMultipleRegisters, Address: addr + uint16(off),
			Quantity: uint16(n), Values: payload,
		}); err != nil {
			return err
		}
		off += n
	}
	return nil
}

func validBitRegion(r Region) bool {
	return r == RegionCoils || r == RegionDiscreteInputs
}

// unpackBits 按 Modbus 位打包规则解出 qty 个布尔值。
func unpackBits(data []byte, qty uint16) ([]bool, error) {
	if len(data) != int((qty+7)/8) {
		return nil, NewError(KindLengthMismatch, "bit payload length disagrees with quantity")
	}
	out := make([]bool, qty)
	for i := range out {
		out[i] = data[i/8]&(1<<uint(i%8)) != 0
	}
	return out, nil
}
