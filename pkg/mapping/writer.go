package mapping

import (
	"reflect"
	"sync"
)

// Writer 在 WriteTransport 之上为并发批量写回提供"下发—确认"整体隔离:
// 同一数据区中地址区间重叠的两个批次,其各自的全部片段(下发、读回确认、
// 必要时的重发)线性串行,绝不互相插入;区间互不相交的批次仍可并发。
//
// 零值不可用,必须经 NewWriter 创建。包级 WriteBatch 按底层传输共享同一组锁,
// 因此多个 goroutine 直接调用 mapping.WriteBatch(client, ...) 同样受隔离保护。
type Writer struct {
	w     WriteTransport
	locks *areaLockSet
}

// NewWriter 基于给定传输创建隔离写回器。
func NewWriter(w WriteTransport) *Writer {
	return &Writer{w: w, locks: newAreaLockSet()}
}

// WriteBatch 在区间隔离下执行批量写回。语义与包级 WriteBatch 完全一致,
// 区别仅在于锁表归属:同一 Writer(或共享同一底层传输的包级调用)上,
// 覆盖区间重叠的两个批次的"下发—读回确认—重发"全过程互不插入。
func (wr *Writer) WriteBatch(reqs []WriteRequest, cfg WriteConfig) (*WriteBatchResult, error) {
	return executeBatch(wr.w, wr.locks, reqs, cfg)
}

// --- 按底层传输共享的锁表注册表 ---

var (
	sharedWritersMu sync.Mutex
	sharedWriters   = map[uintptr]*Writer{}
)

// sharedWriter 返回与底层传输绑定的共享 Writer:同一传输的所有包级
// WriteBatch 调用共用一把区间锁表。map 持有着传输对象,其指针身份不会被复用。
func sharedWriter(w WriteTransport) *Writer {
	key := transportKey(w)
	if key == 0 {
		// 取不到稳定身份(基于不可比较/非引用具体类型的自定义实现):
		// 返回独立写回器,功能完整,只是不与其他调用共享区间锁。
		return NewWriter(w)
	}
	sharedWritersMu.Lock()
	defer sharedWritersMu.Unlock()
	wr, ok := sharedWriters[key]
	if !ok {
		wr = NewWriter(w)
		sharedWriters[key] = wr
	}
	return wr
}

// transportKey 提取引用类型传输的稳定身份;非引用类型返回 0。
func transportKey(w WriteTransport) uintptr {
	rv := reflect.ValueOf(w)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return rv.Pointer()
	}
	return 0
}

// heldInterval 是一个数据区内当前被某批次占有的写区间 [start, end)。
type heldInterval struct {
	area       Area
	start, end uint16
	done       chan struct{}
}

// overlaps 判断两个半开区间是否相交。
func (h *heldInterval) overlaps(start, end uint16) bool {
	return start < h.end && h.start < end
}

// areaLockSet 是四个数据区各自的写区间集合(互斥区间表)。
type areaLockSet struct {
	mu   sync.Mutex
	held map[Area][]*heldInterval
	// waitHook 仅测试使用:当非 nil 时,冲突批次会在确认冲突后调用它取得一个
	// "已挂起"通道,并在真正阻塞等待前先等该通道关闭 —— 钩子由此获得
	// "等待方即将挂起"的确定性同步点(可先让占用方继续)。生产路径恒为 nil,
	// 等待退化为普通 <-done,无额外 goroutine 或开销。
	waitHook func(area Area, start, end uint16) (parked <-chan struct{})
}

func newAreaLockSet() *areaLockSet {
	return &areaLockSet{held: map[Area][]*heldInterval{}}
}

// acquire 占有指定数据区的 [start,end) 写区间,阻塞至区间与所有既存区间不相交;
// 返回释放函数。被唤醒后重新竞争并重新检查冲突(防止放行期间区间被新批次占走)。
func (s *areaLockSet) acquire(area Area, start, end uint16) func() {
	for {
		s.mu.Lock()
		list := s.held[area]
		var block *heldInterval
		for _, h := range list {
			if h.overlaps(start, end) {
				block = h
				break
			}
		}
		if block == nil {
			h := &heldInterval{area: area, start: start, end: end, done: make(chan struct{})}
			s.held[area] = append(list, h)
			s.mu.Unlock()
			return func() { s.release(h) }
		}
		wait := block.done
		s.mu.Unlock()
		if s.waitHook != nil {
			// 钩子通道关闭前,等待方不会消费 wait,占用方仍可安全完成确认。
			if parked := s.waitHook(area, start, end); parked != nil {
				<-parked
			}
		}
		<-wait
	}
}

// release 移除区间并唤醒等待者。
func (s *areaLockSet) release(h *heldInterval) {
	s.mu.Lock()
	list := s.held[h.area]
	for i, v := range list {
		if v == h {
			s.held[h.area] = append(list[:i], list[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	close(h.done)
}

// areaGroup 是一个数据区内的全部写入片段(已按地址升序)。
type areaGroup struct {
	area Area
	segs []int // Segment 在 segs 中的下标
}

// groupByArea 按数据区把片段分组(区域顺序固定为 Area 枚举序)。
// PlanWriteSegments 已按 Area 枚举序、组内地址升序产出片段。
func groupByArea(segs []Segment) []areaGroup {
	idx := map[Area]int{}
	var groups []areaGroup
	for si, seg := range segs {
		gi, ok := idx[seg.Area]
		if !ok {
			idx[seg.Area] = len(groups)
			groups = append(groups, areaGroup{area: seg.Area, segs: []int{si}})
			continue
		}
		groups[gi].segs = append(groups[gi].segs, si)
	}
	return groups
}

// acquireGroup 一次性获取一个数据区内本批全部片段的写区间(按地址升序,
// 所有批次都按同一全局顺序获取 -> 无死锁)并整批持有:本批在该区的全部
// "下发—读回确认—重发"完成前不释放,重叠批次无法插入;片段间的间隙地址
// 不属任何片段,其他批次仍可并发写间隙。返回逆序释放函数(LIFO)。
func (s *areaLockSet) acquireGroup(g areaGroup, segs []Segment) func() {
	releases := make([]func(), 0, len(g.segs))
	for _, si := range g.segs {
		seg := segs[si]
		releases = append(releases, s.acquire(g.area, seg.Address, seg.Address+seg.Quantity))
	}
	return func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
}
