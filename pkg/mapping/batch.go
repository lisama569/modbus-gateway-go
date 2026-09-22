package mapping

import "sort"

// 各数据区单次物理读取的容量上限(Modbus 规范)。
const (
	maxBitsRead      = 2000 // FC01/FC02
	maxRegistersRead = 125  // FC03/FC04
	maxAddress       = 65536
)

func isBitArea(a Area) bool { return a == AreaCoils || a == AreaDiscreteInputs }

// pointWidth 返回点位在其数据区中占用的单元数(寄存器数或位数)。
func pointWidth(p Point) uint16 {
	if isBitArea(p.Area) {
		return 1
	}
	return p.Type.Size()
}

// MapError 是映射/批量读取层的错误,携带确定性的归一化原因。
type MapError struct {
	// Kind 为 "address" | "plan" | "decode" | "transport" | "partial"
	Kind string
	Msg  string
}

func (e *MapError) Error() string { return "mapping: [" + e.Kind + "] " + e.Msg }

// PlanSegments 按确定规则把点位拆分为物理读取片段:
//
//  1. 按数据区分组:跨区请求绝不合并(功能码与寄存器空间都不同)。
//  2. 组内按起始地址升序(同地址按原始下标稳定排序)。
//  3. 以"当前帧的第一个点位地址"为帧起点贪心合并:
//     当且仅当"候选点位起点 + 宽度 - 帧起点 <= 单帧容量"才并入;
//     一旦某点位(含宽点位)放不下,立即另开一帧,宽点位绝不跨帧拆分。
//     帧内间隙地址随合并被一并读出,但不会仅为了覆盖间隙而扩大拆分。
//  4. 同一点位集合的拆分结果完全确定,与输入顺序无关。
func PlanSegments(points []Point) ([]Segment, error) {
	if len(points) == 0 {
		return nil, &MapError{Kind: "plan", Msg: "no points to read"}
	}

	// 校验并生成排序后的(下标,点位)序列,按区分桶。
	buckets := map[Area][]int{}
	for i, p := range points {
		w := pointWidth(p)
		if w == 0 {
			return nil, &MapError{Kind: "plan", Msg: "point " + p.Name + " has zero width"}
		}
		if isBitArea(p.Area) && p.Type != TypeBool {
			return nil, &MapError{Kind: "plan",
				Msg: "point " + p.Name + ": non-bool type cannot map to bit area"}
		}
		if uint64(p.Address)+uint64(w) > maxAddress {
			return nil, &MapError{Kind: "address",
				Msg: "point " + p.Name + ": address range overflows 0..65535"}
		}
		buckets[p.Area] = append(buckets[p.Area], i)
	}

	var segs []Segment
	// 区域遍历顺序也固定:按 Area 枚举值。
	for area := AreaCoils; area <= AreaInputRegisters; area++ {
		idxs := buckets[area]
		if idxs == nil {
			continue
		}
		sort.SliceStable(idxs, func(a, b int) bool {
			pa, pb := points[idxs[a]], points[idxs[b]]
			if pa.Address != pb.Address {
				return pa.Address < pb.Address
			}
			return idxs[a] < idxs[b]
		})

		cap := uint16(maxRegistersRead)
		if isBitArea(area) {
			cap = maxBitsRead
		}

		var cur *Segment
		flush := func() {
			if cur != nil {
				segs = append(segs, *cur)
				cur = nil
			}
		}
		for _, idx := range idxs {
			p := points[idx]
			w := pointWidth(p)
			end := p.Address + w
			if cur == nil || end-cur.Address > cap {
				// 放不下(以帧起点计容量):开新帧。单个点位最大 4 字,不会超过容量。
				flush()
				cur = &Segment{Area: area, Address: p.Address, Quantity: w}
			} else if end-cur.Address > cur.Quantity {
				cur.Quantity = end - cur.Address
			}
			cur.Points = append(cur.Points, idx)
		}
		flush()
	}
	return segs, nil
}

// ReadBatch 按确定规则拆分点位并批量读取,汇总成功部分与整体结论:
//   - 每个片段独立读取;某片段失败只令该片段覆盖的点位失败,其余片段照常完成。
//   - Results 与输入点位一一对应(同序),每个结果独立携带值或错误。
//   - 返回的 error 仅在存在失败点位时非空(归一化为 partial 结论),
//     已成功的点位仍完整保留在 BatchResult 中。
func ReadBatch(r RegisterReader, points []Point) (*BatchResult, error) {
	segs, err := PlanSegments(points)
	if err != nil {
		return nil, err
	}

	res := &BatchResult{
		Results:       make([]PointResult, len(points)),
		Segments:      segs,
		SegmentErrors: make([]error, len(segs)),
	}
	for i := range points {
		res.Results[i] = PointResult{Index: i, Point: points[i]}
	}

	for si, seg := range segs {
		bits, regs, rerr := readSegment(r, seg)
		if rerr != nil {
			res.SegmentErrors[si] = rerr
			for _, pi := range seg.Points {
				res.Results[pi].Err = &MapError{
					Kind: "transport",
					Msg: "segment @" + areaName(seg.Area) + "+" +
						itoaLenM(int(seg.Address)) + " failed: " + rerr.Error(),
				}
			}
			continue
		}

		for _, pi := range seg.Points {
			p := points[pi]
			off := int(p.Address - seg.Address)
			pr := &res.Results[pi]
			if isBitArea(seg.Area) {
				pr.Value = bits[off]
			} else {
				w := int(p.Type.Size())
				slice := regs[off : off+w]
				v, derr := DecodeValue(p.Type, p.Order, slice)
				if derr != nil {
					pr.Err = &MapError{Kind: "decode", Msg: "point " + p.Name + ": " + derr.Error()}
					continue
				}
				pr.Raw = encodeRegisters(BigEndian, slice)
				pr.Value = v
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
			Msg: itoaLenM(res.Succeeded) + " point(s) succeeded, " +
				itoaLenM(res.Failed) + " failed"}
	}
	return res, nil
}

// readSegment 读取一个片段,依区域返回位数组或寄存器数组。
func readSegment(r RegisterReader, seg Segment) ([]bool, []uint16, error) {
	switch seg.Area {
	case AreaCoils:
		b, err := r.ReadCoils(seg.Address, seg.Quantity)
		return b, nil, err
	case AreaDiscreteInputs:
		b, err := r.ReadDiscreteInputs(seg.Address, seg.Quantity)
		return b, nil, err
	case AreaHoldingRegisters:
		v, err := r.ReadHoldingRegisters(seg.Address, seg.Quantity)
		return nil, v, err
	case AreaInputRegisters:
		v, err := r.ReadInputRegisters(seg.Address, seg.Quantity)
		return nil, v, err
	}
	return nil, nil, &MapError{Kind: "plan", Msg: "unknown area"}
}

func areaName(a Area) string {
	switch a {
	case AreaCoils:
		return "coils"
	case AreaDiscreteInputs:
		return "discrete"
	case AreaHoldingRegisters:
		return "holding"
	case AreaInputRegisters:
		return "input"
	}
	return "unknown"
}
