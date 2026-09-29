package mapping

import "sort"

// 写操作单帧容量上限(Modbus 规范)。
const (
	// maxCoilsWrite FC0F 单帧最多 1968 个位。
	maxCoilsWrite = 1968
	// maxRegistersWrite FC10 单帧最多 123 个寄存器。
	maxRegistersWrite = 123
)

// WriteItem 是一个待写入点位:点位配置 + 归一化值。
type WriteItem struct {
	Point Point
	// Value 的可接受 Go 类型:
	//   bool(线圈);uint16/int16/uint32/int32/float32/uint64/int64/float64
	// 与 Point.Type 对应,类型不符按单项失败处理。
	Value any
}

// WriteResult 是单个点位的写入结果,与输入同序。
type WriteResult struct {
	Index int
	Point Point
	// Err 非 nil 表示该点位最终未确认成功:
	//   "plan"      只读区域/类型与宽度不符/地址溢出/同批点位地址重叠
	//   "value"     值类型与点位类型不符或超出表示范围
	//   "transport" 底层写/确认失败(对端明确拒绝等)
	//   "uncertain" 恢复确认用尽仍无法判定,值可能已生效
	Err error
	// Attempts 该点位物理写入尝试次数(确认读不计入)。
	Attempts int
	// Confirmed 为 true 表示最终状态已经过读回确认(含"首次响应丢失后确认")。
	Confirmed bool
}

// WriteBatchResult 是一次批量写入的整体结论。
type WriteBatchResult struct {
	// OK 为 true 当且仅当所有点位成功确认。
	OK bool
	// Results 与输入点位一一对应(同序)。
	Results []WriteResult
	// Succeeded/Failed 成功/失败点位数。
	Succeeded int
	Failed    int
	// Segments 实际执行的写片段(校验失败的点位不进入任何片段)。
	Segments []WriteSegment
	// SegmentErrors 与 Segments 一一对应(nil 表示该片段成功)。
	SegmentErrors []error
}

// WriteSegment 是一次物理写入片段。
type WriteSegment struct {
	Area    Area
	Address uint16
	// Quantity 位区域为位数,寄存器区域为寄存器数。
	Quantity uint16
	// Points 覆盖的输入点位下标(段内按地址升序)。
	Points []int
}

// RegisterWriter 是批量写入依赖的底层传输能力。
// 两个方法都必须提供"安全写入语义":
// 对端明确拒绝立即失败;仅超时/连接失效等不确定情况才允许恢复,
// 恢复通过读回确认最终状态,迟到响应不得混入新结果。
type RegisterWriter interface {
	WriteCoils(addr uint16, bits []bool) (attempts int, err error)
	WriteRegisters(addr uint16, regs []uint16) (attempts int, err error)
}

// encoded 是通过校验、可进入写规划的点位内部表示。
type encoded struct {
	index int
	point Point
	width uint16
	bits  []bool // 仅线圈:单元素
	regs  []uint16
}

// WriteBatch 按确定规则把写入点位拆分为物理写片段,逐段执行并汇总:
//   - 只写线圈与保持寄存器;只读区域、宽度/类型不符、同批地址重叠的点位
//     作为单项失败,绝不静默跳过或写到别的点位。
//   - 跨区域不合并;超过单帧容量按与读取一致的贪心规则拆分;
//     结果与输入同序,片段失败只影响该片段覆盖的点位,已成功点位保留。
//   - 返回的 error 仅在存在失败点位时非空(partial 结论)。
func WriteBatch(w RegisterWriter, items []WriteItem) (*WriteBatchResult, error) {
	res := &WriteBatchResult{Results: make([]WriteResult, len(items))}
	for i := range items {
		res.Results[i] = WriteResult{Index: i, Point: items[i].Point}
	}
	if len(items) == 0 {
		return nil, &MapError{Kind: "plan", Msg: "no points to write"}
	}

	// 1) 逐项校验 + 编码。
	enc := make([]*encoded, 0, len(items))
	for i, it := range items {
		e, err := validateAndEncode(i, it)
		if err != nil {
			res.Results[i].Err = err
			continue
		}
		enc = append(enc, e)
	}

	// 2) 同区域地址重叠检测:重叠点位全部拒绝,避免后写覆盖先写。
	markOverlaps(enc, res)
	planned := make([]*encoded, 0, len(enc))
	for _, e := range enc {
		if res.Results[e.index].Err == nil {
			planned = append(planned, e)
		}
	}

	// 3) 确定性拆分(与读取相同的区域序、地址序、贪心规则)。
	segs, segPayload := planWriteSegments(planned)
	res.Segments = segs
	res.SegmentErrors = make([]error, len(segs))

	// 4) 逐段执行。
	for si, seg := range segs {
		var attempts int
		var werr error
		switch seg.Area {
		case AreaCoils:
			attempts, werr = w.WriteCoils(seg.Address, segPayload[si].([]bool))
		case AreaHoldingRegisters:
			attempts, werr = w.WriteRegisters(seg.Address, segPayload[si].([]uint16))
		default:
			werr = &MapError{Kind: "plan", Msg: "unwritable area"}
		}
		if attempts < 1 {
			attempts = 1
		}
		res.SegmentErrors[si] = werr
		for _, pi := range seg.Points {
			pr := &res.Results[pi]
			if werr == nil {
				pr.Confirmed = true
				pr.Attempts = attempts
				continue
			}
			pr.Err = classifyWriteErr(items[pi].Point, werr)
			if a, ok := attemptsFrom(werr); ok {
				pr.Attempts = a
			} else {
				pr.Attempts = attempts
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

// validateAndEncode 校验单个写入点位并完成值编码。
func validateAndEncode(index int, it WriteItem) (*encoded, error) {
	p := it.Point
	e := &encoded{index: index, point: p}
	switch p.Area {
	case AreaCoils:
		if p.Type != TypeBool {
			return nil, &MapError{Kind: "plan",
				Msg: "point " + p.Name + ": non-bool type cannot map to coils"}
		}
		on, ok := it.Value.(bool)
		if !ok {
			return nil, &MapError{Kind: "value",
				Msg: "point " + p.Name + ": value type " + goTypeName(it.Value) +
					" does not match bool coil"}
		}
		e.width = 1
		e.bits = []bool{on}
	case AreaHoldingRegisters:
		if p.Type == TypeBool {
			return nil, &MapError{Kind: "plan",
				Msg: "point " + p.Name + ": bool cannot map to holding registers"}
		}
		regs, err := EncodeValue(p.Type, p.Order, it.Value)
		if err != nil {
			return nil, &MapError{Kind: "value", Msg: "point " + p.Name + ": " + err.Error()}
		}
		e.width = uint16(len(regs))
		e.regs = regs
	case AreaDiscreteInputs, AreaInputRegisters:
		return nil, &MapError{Kind: "plan",
			Msg: "point " + p.Name + ": " + areaName(p.Area) + " is read-only"}
	default:
		return nil, &MapError{Kind: "plan",
			Msg: "point " + p.Name + ": unknown area"}
	}
	if uint64(p.Address)+uint64(e.width) > maxAddress {
		return nil, &MapError{Kind: "plan",
			Msg: "point " + p.Name + ": address range overflows 0..65535"}
	}
	return e, nil
}

// markOverlaps 标记同区域内地址范围相交的所有点位为失败(含自身重复地址)。
func markOverlaps(enc []*encoded, res *WriteBatchResult) {
	byArea := map[Area][]*encoded{}
	for _, e := range enc {
		byArea[e.point.Area] = append(byArea[e.point.Area], e)
	}
	bad := map[int]bool{}
	for _, list := range byArea {
		sort.Slice(list, func(i, j int) bool {
			if list[i].point.Address != list[j].point.Address {
				return list[i].point.Address < list[j].point.Address
			}
			return list[i].index < list[j].index
		})
		for i := 1; i < len(list); i++ {
			prev, cur := list[i-1], list[i]
			if cur.point.Address < prev.point.Address+prev.width {
				bad[cur.index] = true
				bad[prev.index] = true
			}
		}
	}
	for idx := range bad {
		p := res.Results[idx].Point
		res.Results[idx].Err = &MapError{Kind: "plan",
			Msg: "point " + p.Name + ": address range overlaps another write point"}
	}
}

// planWriteSegments 按确定规则拆分写点位,同时构造每段的连续载荷。
// 只合并"相邻(含)且连续"的点位:写覆盖范围必须恰好等于目标点位集合,
// 不会为了合并而顺带改写任何间隙地址。
func planWriteSegments(enc []*encoded) ([]WriteSegment, []any) {
	buckets := map[Area][]*encoded{}
	for _, e := range enc {
		buckets[e.point.Area] = append(buckets[e.point.Area], e)
	}

	var segs []WriteSegment
	var payloads []any
	for area := AreaCoils; area <= AreaInputRegisters; area++ {
		list := buckets[area]
		if list == nil {
			continue
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].point.Address != list[j].point.Address {
				return list[i].point.Address < list[j].point.Address
			}
			return list[i].index < list[j].index
		})

		cap := uint16(maxCoilsWrite)
		if area == AreaHoldingRegisters {
			cap = maxRegistersWrite
		}

		var cur []*encoded
		flush := func() {
			if len(cur) == 0 {
				return
			}
			seg := WriteSegment{Area: area, Address: cur[0].point.Address}
			last := cur[len(cur)-1]
			seg.Quantity = last.point.Address + last.width - seg.Address
			idxs := make([]int, 0, len(cur))
			for _, e := range cur {
				idxs = append(idxs, e.index)
			}
			seg.Points = idxs
			segs = append(segs, seg)
			payloads = append(payloads, buildPayload(area, cur, seg.Address, seg.Quantity))
			cur = nil
		}

		for _, e := range list {
			if len(cur) == 0 {
				cur = []*encoded{e}
				continue
			}
			first := cur[0]
			last := cur[len(cur)-1]
			contiguous := e.point.Address == last.point.Address+last.width
			span := e.point.Address + e.width - first.point.Address
			if !contiguous || span > cap {
				flush()
			}
			cur = append(cur, e)
		}
		flush()
	}
	return segs, payloads
}

// buildPayload 生成片段在地址区间内的连续位/寄存器载荷。
func buildPayload(area Area, cur []*encoded, base, qty uint16) any {
	if area == AreaCoils {
		bits := make([]bool, qty)
		for _, e := range cur {
			bits[e.point.Address-base] = e.bits[0]
		}
		return bits
	}
	regs := make([]uint16, qty)
	for _, e := range cur {
		copy(regs[e.point.Address-base:], e.regs)
	}
	return regs
}

func attemptsFrom(err error) (int, bool) {
	var ac AttemptCarrier
	for err != nil {
		if a, ok := err.(AttemptCarrier); ok {
			return a.WriteAttempts(), true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return 0, false
		}
		err = u.Unwrap()
	}
	_ = ac
	return 0, false
}
