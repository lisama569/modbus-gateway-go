package mapping

import (
	"errors"
	"sort"
)

// 各可写数据区单次物理写入的容量上限(Modbus 规范)。
const (
	maxCoilsWrite     = 1968 // FC0F
	maxRegistersWrite = 123  // FC10
)

// WriteRequest 是单个点位的写回请求:值必须与 Point.Type 兼容。
type WriteRequest struct {
	Point Point
	// Value 支持 bool 及 int16/uint16/int32/uint32/int64/uint64/float32/float64,
	// 与 Point.Type 不匹配按单项失败处理,绝不静默改写。
	Value any
}

// WriteResult 是单个点位的写回结果,与输入同序。
type WriteResult struct {
	Index int
	Point Point
	Value any
	// Attempts 是该点位所属片段的物理下发次数(含首次)。
	Attempts int
	// Confirmed 为 true 表示成功结论来自"响应丢失后的读回确认",
	// 而非对端明确应答。
	Confirmed bool
	Err       error
}

// WriteBatchResult 是一次批量写回的整体结论。
type WriteBatchResult struct {
	// OK 为 true 当且仅当全部点位写成功(含读回确认成功)。
	OK bool
	// Results 与输入请求一一对应(同序)。
	Results   []WriteResult
	Succeeded int
	Failed    int
	// Segments 是确定性拆分后的写入片段;校验失败的点位不属于任何片段。
	Segments      []Segment
	SegmentErrors []error
}

// ConfirmPolicy 控制不确定写入后是否读回确认最终状态。
type ConfirmPolicy int

const (
	// ConfirmDefault 零值:采用安全默认(读回确认开启)。
	ConfirmDefault ConfirmPolicy = iota
	// ConfirmOff 关闭读回确认:不确定失败直接在次数内重发。
	ConfirmOff
	// ConfirmOn 显式开启读回确认。
	ConfirmOn
)

// WriteConfig 控制写回恢复与确认语义。
type WriteConfig struct {
	// MaxAttempts 单个片段的最多物理下发次数(含首次);<=0 取默认值 3。
	MaxAttempts int
	// Confirm 控制不确定失败(超时/连接失效等)后是否读回比对确认最终状态;
	// 零值 ConfirmDefault 与 ConfirmOn 都开启(安全默认),ConfirmOff 关闭。
	Confirm ConfirmPolicy
}

func (c WriteConfig) withDefaults() WriteConfig {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	return c
}

// confirmEnabled 返回是否启用读回确认。
func (c WriteConfig) confirmEnabled() bool { return c.Confirm != ConfirmOff }

// RegisterWriter 是写回依赖的底层写能力。
type RegisterWriter interface {
	WriteSingleCoil(addr uint16, on bool) error
	WriteSingleRegister(addr, value uint16) error
	WriteMultipleCoils(addr uint16, bits []bool) error
	WriteMultipleRegisters(addr uint16, regs []uint16) error
}

// WriteTransport 是批量写回依赖的底层传输:写 + 读回确认。
// *modbus.Client 天然满足该接口。
type WriteTransport interface {
	RegisterWriter
	RegisterReader
}

// PlanWriteSegments 校验写回请求并按确定规则拆分写入片段:
//
//  1. 只读区(离散输入/输入寄存器)直接判单项失败,绝不生成写片段。
//  2. 值类型必须与点位类型严格一致(不做隐式转换),宽度/地址范围非法同样单项失败。
//  3. 任意两个合法点位的写入区间重叠时,冲突双方(全部相关点)都判单项失败,
//     避免同一帧内对同一地址给出两个值,也避免漏写其中一个。
//  4. 仅可写线圈区/保持寄存器区参与规划,跨区绝不合并。
//  5. 组内按地址升序(同地址按原始下标)稳定排序;仅当点位地址与当前帧尾
//     严格连续(无间隙)且并入后不超过单帧容量时才合并 —— 间隙地址不属于
//     任何请求,绝不被顺带改写。宽点位绝不跨帧拆分。
//  6. 同一点位集合的拆分结果完全确定,与输入顺序无关。
//
// 返回的 pointErrs 与输入请求同序,nil 表示该点位合法;非法点位不出现在任何片段中。
func PlanWriteSegments(reqs []WriteRequest) (segs []Segment, pointErrs []error) {
	pointErrs = make([]error, len(reqs))

	// 逐点静态校验。
	valid := make([]bool, len(reqs))
	for i, r := range reqs {
		if err := validateWrite(r); err != nil {
			pointErrs[i] = err
			continue
		}
		valid[i] = true
	}

	// 重叠检测:对合法点两两判定区间相交,冲突的全部点失败。
	for i := range reqs {
		if !valid[i] {
			continue
		}
		for j := i + 1; j < len(reqs); j++ {
			if !valid[j] || reqs[i].Point.Area != reqs[j].Point.Area {
				continue
			}
			a, pa := reqs[i].Point, pointWidth(reqs[i].Point)
			b, pb := reqs[j].Point, pointWidth(reqs[j].Point)
			if a.Address < b.Address+pb && b.Address < a.Address+pa {
				if pointErrs[i] == nil {
					pointErrs[i] = &MapError{Kind: "plan",
						Msg: "write point " + a.Name + " overlaps with another write point"}
				}
				pointErrs[j] = &MapError{Kind: "plan",
					Msg: "write point " + b.Name + " overlaps with another write point"}
			}
		}
		if pointErrs[i] != nil {
			valid[i] = false
		}
	}

	// 按区分桶合法点位。
	buckets := map[Area][]int{}
	for i := range reqs {
		if valid[i] {
			buckets[reqs[i].Point.Area] = append(buckets[reqs[i].Point.Area], i)
		}
	}

	for area := AreaCoils; area <= AreaInputRegisters; area++ {
		idxs := buckets[area]
		if idxs == nil {
			continue
		}
		sort.SliceStable(idxs, func(a, b int) bool {
			pa, pb := reqs[idxs[a]].Point, reqs[idxs[b]].Point
			if pa.Address != pb.Address {
				return pa.Address < pb.Address
			}
			return idxs[a] < idxs[b]
		})

		cap := uint16(maxRegistersWrite)
		if area == AreaCoils {
			cap = maxCoilsWrite
		}

		var cur *Segment
		flush := func() {
			if cur != nil {
				segs = append(segs, *cur)
				cur = nil
			}
		}
		for _, idx := range idxs {
			p := reqs[idx].Point
			w := pointWidth(p)
			end := p.Address + w
			// 写回只合并"地址严格连续"的点位:候选起点必须正好是当前帧尾,
			// 且并入后不超容量;否则另开新帧(宽点位整体进入新帧)。
			if cur == nil || p.Address != cur.Address+cur.Quantity ||
				end-cur.Address > cap {
				flush()
				cur = &Segment{Area: area, Address: p.Address, Quantity: w}
			} else {
				cur.Quantity = end - cur.Address
			}
			cur.Points = append(cur.Points, idx)
		}
		flush()
	}
	return segs, pointErrs
}

// validateWrite 校验单个写回请求的区域、类型、值类型与地址范围。
func validateWrite(r WriteRequest) error {
	p := r.Point
	switch p.Area {
	case AreaDiscreteInputs:
		return &MapError{Kind: "readonly",
			Msg: "point " + p.Name + ": discrete inputs are read-only"}
	case AreaInputRegisters:
		return &MapError{Kind: "readonly",
			Msg: "point " + p.Name + ": input registers are read-only"}
	case AreaCoils:
		if p.Type != TypeBool {
			return &MapError{Kind: "plan",
				Msg: "point " + p.Name + ": coil point must be TypeBool"}
		}
		if _, ok := r.Value.(bool); !ok {
			return &MapError{Kind: "value",
				Msg: "point " + p.Name + ": coil value must be bool"}
		}
	case AreaHoldingRegisters:
		if p.Type == TypeBool {
			return &MapError{Kind: "plan",
				Msg: "point " + p.Name + ": bool cannot map to holding register"}
		}
		// 试编码同时完成值类型与宽度校验。
		if _, err := EncodeValue(p.Type, p.Order, r.Value); err != nil {
			return &MapError{Kind: "value", Msg: "point " + p.Name + ": " + err.Error()}
		}
	default:
		return &MapError{Kind: "plan", Msg: "point " + p.Name + ": unknown area"}
	}
	w := pointWidth(p)
	if w == 0 {
		return &MapError{Kind: "plan", Msg: "point " + p.Name + " has zero width"}
	}
	if uint64(p.Address)+uint64(w) > maxAddress {
		return &MapError{Kind: "address",
			Msg: "point " + p.Name + ": address range overflows 0..65535"}
	}
	return nil
}

// retryableErr 用结构化断言识别"不确定"错误(不直接依赖 modbus 包)。
// *modbus.Error 实现了 Retryable() bool;其他无法判定的错误一律视为不确定,
// 走读回确认而非直接判失败。
type retryAble interface{ Retryable() bool }

func isRetryable(err error) bool {
	var r retryAble
	if errors.As(err, &r) {
		return r.Retryable()
	}
	return true
}

// WriteBatch 按点位配置批量写回,带确定性拆分、单项失败、恢复与确认语义:
//
//   - 静态校验失败(只读区/类型不符/重叠)的点位立即得到单项错误,不产生任何下发。
//   - 每个片段独立下发;片段失败只令该片段覆盖的点位失败,已成功片段保留。
//   - 明确拒绝(异常响应、非法地址/数据值、不支持功能码)不重试,直接失败;
//     不确定错误(超时/连接失效/帧错误)先读回比对确认:
//   - 读回值与本片段目标载荷一致 -> 判成功(Confirmed=true,Attempts 记录下发次数),
//     即"请求可能已生效但响应丢失"不会被误报为失败;
//   - 读回值不一致或读回本身失败 -> 在最大次数内重新下发(绝对值写,幂等安全);
//   - 全部次数耗尽且无法确认 -> 判失败,绝不把未确认生效的写入报成成功。
//   - 结果与输入同序,整体结论用 OK 区分全部成功/部分失败(返回 partial MapError)。
func WriteBatch(w WriteTransport, reqs []WriteRequest, cfg WriteConfig) (*WriteBatchResult, error) {
	cfg = cfg.withDefaults()
	if len(reqs) == 0 {
		return nil, &MapError{Kind: "plan", Msg: "no points to write"}
	}

	segs, pointErrs := PlanWriteSegments(reqs)
	res := &WriteBatchResult{
		Results:       make([]WriteResult, len(reqs)),
		Segments:      segs,
		SegmentErrors: make([]error, len(segs)),
	}
	for i := range reqs {
		res.Results[i] = WriteResult{Index: i, Point: reqs[i].Point, Value: reqs[i].Value}
		if pointErrs[i] != nil {
			res.Results[i].Err = pointErrs[i]
		}
	}

	for si, seg := range segs {
		attempts, confirmed, werr := writeSegment(w, seg, reqs, cfg)
		res.SegmentErrors[si] = werr
		for _, pi := range seg.Points {
			pr := &res.Results[pi]
			pr.Attempts = attempts
			pr.Confirmed = confirmed
			if werr != nil {
				pr.Err = &MapError{
					Kind: "transport",
					Msg: "write segment @" + areaName(seg.Area) + "+" +
						itoaLenM(int(seg.Address)) + " failed after " +
						itoaLenM(attempts) + " attempt(s): " + werr.Error(),
				}
			}
		}
	}

	for i := range res.Results {
		if res.Results[i].Err != nil {
			res.Failed++
		} else {
			res.Succeeded++
		}
	}
	res.OK = res.Failed == 0
	if !res.OK {
		return res, &MapError{Kind: "partial",
			Msg: itoaLenM(res.Succeeded) + " point(s) written, " +
				itoaLenM(res.Failed) + " failed"}
	}
	return res, nil
}

// writeSegment 执行一个写入片段的完整"下发-确认"循环。
// 返回物理下发次数、成功是否来自读回确认、最终错误(nil 表示成功)。
func writeSegment(w WriteTransport, seg Segment, reqs []WriteRequest,
	cfg WriteConfig) (attempts int, confirmed bool, err error) {
	var lastErr error
	for attempts = 1; attempts <= cfg.MaxAttempts; attempts++ {
		werr := issueSegmentWrite(w, seg, reqs)
		if werr == nil {
			return attempts, false, nil // 对端明确应答
		}
		lastErr = werr
		if !isRetryable(werr) {
			// 明确拒绝:绝不再次下发。
			return attempts, false, werr
		}
		if cfg.confirmEnabled() {
			// 不确定:先读回确认,避免把已生效的控制量误报失败,
			// 也避免无意义的重复下发。
			ok, _ := segmentConfirmed(w, seg, reqs)
			if ok {
				return attempts, true, nil
			}
			// 值不一致或读回也失败:继续下一轮重发(幂等绝对值写)。
		}
		// 未开启确认或未确认成功:继续下一轮重发。
		lastErr = &MapError{Kind: "uncertain",
			Msg: "write outcome unconfirmed: " + werr.Error()}
	}
	// 次数耗尽前再做一次读回(仅确认模式):最后一帧仍可能已生效。
	if cfg.confirmEnabled() {
		if ok, _ := segmentConfirmed(w, seg, reqs); ok {
			return cfg.MaxAttempts, true, nil
		}
	}
	return cfg.MaxAttempts, false, lastErr
}

// issueSegmentWrite 按片段区域与宽度选择功能码下发一次:
// 单点用 0x05/0x06,连续多点用 0x0F/0x10。
func issueSegmentWrite(w WriteTransport, seg Segment, reqs []WriteRequest) error {
	if seg.Area == AreaCoils {
		bits := make([]bool, seg.Quantity)
		for _, pi := range seg.Points {
			r := reqs[pi]
			bits[r.Point.Address-seg.Address] = r.Value.(bool)
		}
		if seg.Quantity == 1 {
			return w.WriteSingleCoil(seg.Address, bits[0])
		}
		return w.WriteMultipleCoils(seg.Address, bits)
	}
	regs := make([]uint16, seg.Quantity)
	for _, pi := range seg.Points {
		r := reqs[pi]
		encoded, err := EncodeValue(r.Point.Type, r.Point.Order, r.Value)
		if err != nil {
			return &MapError{Kind: "value", Msg: "point " + r.Point.Name + ": " + err.Error()}
		}
		off := int(r.Point.Address - seg.Address)
		copy(regs[off:off+len(encoded)], encoded)
	}
	if seg.Quantity == 1 {
		return w.WriteSingleRegister(seg.Address, regs[0])
	}
	return w.WriteMultipleRegisters(seg.Address, regs)
}

// segmentConfirmed 读回片段覆盖区间并与期望载荷逐位/逐寄存器比对。
// 读回本身失败时返回错误,由调用方决定继续重发还是判失败。
func segmentConfirmed(w WriteTransport, seg Segment, reqs []WriteRequest) (bool, error) {
	switch seg.Area {
	case AreaCoils:
		bits, err := w.ReadCoils(seg.Address, seg.Quantity)
		if err != nil {
			return false, err
		}
		for _, pi := range seg.Points {
			want := reqs[pi].Value.(bool)
			if bits[reqs[pi].Point.Address-seg.Address] != want {
				return false, nil
			}
		}
		return true, nil
	case AreaHoldingRegisters:
		got, err := w.ReadHoldingRegisters(seg.Address, seg.Quantity)
		if err != nil {
			return false, err
		}
		for _, pi := range seg.Points {
			r := reqs[pi]
			want, err := EncodeValue(r.Point.Type, r.Point.Order, r.Value)
			if err != nil {
				return false, err
			}
			off := int(r.Point.Address - seg.Address)
			for k := range want {
				if got[off+k] != want[k] {
					return false, nil
				}
			}
		}
		return true, nil
	}
	return false, &MapError{Kind: "plan", Msg: "cannot confirm writes on read-only area"}
}
