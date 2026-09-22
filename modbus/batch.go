package modbus

import (
	"context"
	"fmt"
	"sort"
)

// Chunk 是批量拆分后的一个子请求范围（同一区域、连续范围、受 PDU 容量限制）。
type Chunk struct {
	Region   Region
	Address  uint16
	Quantity uint16
}

// PointResult 单个点位的结果。
type PointResult struct {
	Value any
	Err   error
}

// BatchReadResult 是批量读取的逐项结果：
// 成功点位 Value 为解码后的标量（布尔点位为 bool）；
// 失败点位 Err 带可区分错误分类。Overall 汇总整体结论：
// 全部成功为 nil；任一失败为 *Error(KindPartialFailure)，
// 已成功点位的 Value 不受失败影响，仍然保留。
type BatchReadResult struct {
	Items   map[string]PointResult
	Overall error
}

// FailedCount 返回失败点位数。
func (r *BatchReadResult) FailedCount() int {
	n := 0
	for _, it := range r.Items {
		if it.Err != nil {
			n++
		}
	}
	return n
}

// MaxReadBits 单次读线圈/离散输入 PDU 可携带的最大点数。
const MaxReadBits = MaxBitsPerReadPDU

// MaxReadRegisters 单次读寄存器 PDU 可携带的最大寄存器数。
const MaxReadRegisters = MaxRegistersPerReadPDU

// SplitChunks 按确定规则拆分批点：
//  1. 按区域分组（跨越不同区域绝不合并到同一请求）；
//  2. 组内按起始地址升序排序（地址相同按点位名排序，保证完全确定）；
//  3. 顺序合并为连续范围（当前范围的起点固定，终点尽量延伸，
//     相邻点位首尾相接或存在空洞也纳入同一范围——空洞数据在本地读取
//     成本极低且避免拆分碎片化；空洞不会扩大到超出容量）；
//  4. 范围延伸会使单次请求超过 PDU 容量时，按容量边界切成新 Chunk，
//     边界相对当前范围起点对齐，规则完全确定。
func SplitChunks(points []Point) ([]Chunk, error) {
	if len(points) == 0 {
		return nil, NewError(KindInvalidArgument, "no points to split")
	}
	groups := map[Region][]Point{}
	var order []Region
	for _, p := range points {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if _, ok := groups[p.Region]; !ok {
			order = append(order, p.Region)
		}
		groups[p.Region] = append(groups[p.Region], p)
	}
	// 区域也按固定顺序输出。
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	var chunks []Chunk
	for _, region := range order {
		ps := groups[region]
		sort.SliceStable(ps, func(i, j int) bool {
			if ps[i].Address != ps[j].Address {
				return ps[i].Address < ps[j].Address
			}
			return ps[i].Name < ps[j].Name
		})
		limit := uint16(MaxReadBits)
		if region.IsRegister() {
			limit = MaxReadRegisters
		}
		var start, end uint16 // [start, end)
		flush := func() {
			chunks = append(chunks, Chunk{Region: region, Address: start, Quantity: end - start})
		}
		for i, p := range ps {
			var pEnd uint16
			if region.IsRegister() {
				pEnd = p.Address + uint16(p.RegisterCount())
			} else {
				pEnd = p.Address + 1
			}
			if i == 0 {
				start, end = p.Address, pEnd
				continue
			}
			// 同一区域内：若点位在当前范围之后（允许空洞），
			// 且延伸后不超容量，则扩展当前范围。
			if p.Address >= start && pEnd <= end {
				continue // 被覆盖（点位重叠）
			}
			if p.Address >= end && uint32(pEnd)-uint32(start) <= uint32(limit) {
				end = pEnd
				continue
			}
			flush()
			start, end = p.Address, pEnd
		}
		flush()
	}
	// 最后统一按容量边界切分（合并阶段已尽量保证不超限，这里兜底处理
	// 起点未对齐 0 时 range 自身超容量的极端情况）。
	return enforceCapacity(chunks), nil
}

// enforceCapacity 对超过单次报文容量的 Chunk 按容量边界顺序切分。
func enforceCapacity(chunks []Chunk) []Chunk {
	var out []Chunk
	for _, c := range chunks {
		limit := uint16(MaxReadBits)
		if c.Region.IsRegister() {
			limit = MaxReadRegisters
		}
		remaining := c.Quantity
		cur := c.Address
		for remaining > 0 {
			n := remaining
			if n > limit {
				n = limit
			}
			out = append(out, Chunk{Region: c.Region, Address: cur, Quantity: n})
			cur += n
			remaining -= n
		}
	}
	return out
}

// BatchRead 经 client 批量读取并解码全部点位。
// 拆分后的每个 Chunk 独立执行：某个 Chunk 失败只把覆盖到的点位标记失败，
// 其他 Chunk 已成功的值照常返回；整体结论为 PartialFailure。
func BatchRead(ctx context.Context, c *Client, points []Point) (*BatchReadResult, error) {
	if c == nil {
		return nil, NewError(KindInvalidArgument, "nil client")
	}
	chunks, err := SplitChunks(points)
	if err != nil {
		return nil, err
	}
	res := &BatchReadResult{Items: map[string]PointResult{}}
	// 预登记，保证所有点位都有结论。
	for _, p := range points {
		res.Items[p.Name] = PointResult{}
	}

	for _, ch := range chunks {
		covered := pointsCovered(points, ch)
		if ch.Region.IsRegister() {
			words, err := c.ReadRegisters(ctx, ch.Region, ch.Address, ch.Quantity)
			if err != nil {
				for _, p := range covered {
					res.Items[p.Name] = PointResult{Err: err}
				}
				continue
			}
			for _, p := range covered {
				off := int(p.Address - ch.Address)
				n := p.RegisterCount()
				if off+n > len(words) {
					res.Items[p.Name] = PointResult{Err: NewError(KindLengthMismatch,
						"decoded window exceeds chunk data")}
					continue
				}
				v, derr := decodePoint(p, words[off:off+n])
				if derr != nil {
					res.Items[p.Name] = PointResult{Err: derr}
				} else {
					res.Items[p.Name] = PointResult{Value: v}
				}
			}
		} else {
			bits, err := c.ReadBits(ctx, ch.Region, ch.Address, ch.Quantity)
			if err != nil {
				for _, p := range covered {
					res.Items[p.Name] = PointResult{Err: err}
				}
				continue
			}
			for _, p := range covered {
				off := int(p.Address - ch.Address)
				if off >= len(bits) {
					res.Items[p.Name] = PointResult{Err: NewError(KindLengthMismatch,
						"decoded window exceeds chunk bits")}
					continue
				}
				res.Items[p.Name] = PointResult{Value: bits[off]}
			}
		}
	}

	if n := res.FailedCount(); n > 0 {
		res.Overall = &Error{
			Kind:    KindPartialFailure,
			Message: fmt.Sprintf("%d of %d points failed", n, len(points)),
		}
	}
	return res, res.Overall
}

// pointsCovered 返回地址窗口落在 chunk 内的点位（仅按区域/地址判断）。
func pointsCovered(points []Point, ch Chunk) []Point {
	var out []Point
	for _, p := range points {
		if p.Region != ch.Region {
			continue
		}
		var pEnd uint16
		if ch.Region.IsRegister() {
			pEnd = p.Address + uint16(p.RegisterCount())
		} else {
			pEnd = p.Address + 1
		}
		if p.Address >= ch.Address && pEnd <= ch.Address+ch.Quantity {
			out = append(out, p)
		}
	}
	return out
}

// decodePoint 按点位类型/排列解码；支持寄存器内取位。
func decodePoint(p Point, words []uint16) (any, error) {
	if p.BitPoint {
		if len(words) < 1 {
			return nil, NewError(KindLengthMismatch, "bit extraction needs one word")
		}
		return words[0]&(1<<uint(p.Bit)) != 0, nil
	}
	v, err := DecodeValue(p.Type, p.Order, words)
	if err != nil {
		return nil, err
	}
	// 还原为具体类型，避免全部以 float64 呈现。
	switch p.Type {
	case TypeUint16:
		return uint16(v), nil
	case TypeInt16:
		return int16(v), nil
	case TypeUint32:
		return uint32(v), nil
	case TypeInt32:
		return int32(v), nil
	case TypeFloat32:
		return float32(v), nil
	case TypeUint64:
		bits, _ := DecodeRaw(p.Type, p.Order, words)
		return bits, nil
	case TypeInt64:
		bits, _ := DecodeRaw(p.Type, p.Order, words)
		return int64(bits), nil
	case TypeFloat64:
		return v, nil
	default:
		return v, nil
	}
}
