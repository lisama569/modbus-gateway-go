package mapping

import "sync"

// BatchGuard 由底层传输可选实现:为同一传输上的批量写回提供批次级互斥。
//
// 背景:仅靠底层"单请求串行锁"只能保证单次请求-响应不交叉,无法保证一个批次
// 完整的"下发—(读回确认)—(重发)"循环不被另一批次插入。控制回路并发下发同一片
// 区域时,若 A 批的恢复重发夹在 B 批的下发与读回之间,会把 B 刚写成功的控制量
// 覆盖回去,而双方都可能自报成功。实现该接口后,映射层会把每个批次的全部物理
// 操作(含读回确认与重发)放在 Lock/Unlock 之间,使同一传输上的批次严格串行。
//
// *modbus.Client 实现该接口(复用会话级串行锁);未实现该接口的传输(如测试用
// 内存伪传输)退回到映射层的全局批次锁,仍保证批次原子性,只是不同传输之间也会
// 被一并串行化(仅影响吞吐,不影响正确性)。
type BatchGuard interface {
	LockBatch()
	UnlockBatch()
}

// batchLocks 为未实现 BatchGuard 的传输维护"每传输一把"的批次锁。
// 键使用指针标识;非指针传输(罕见)退化为全局共享一把锁。
var (
	globalBatchMu sync.Mutex
	batchLocks    sync.Map // any(传输标识) -> *sync.Mutex
)

// lockBatchFor 获取传输对应的批次锁并加锁,返回解锁函数(立即调用)。
func lockBatchFor(w WriteTransport) func() {
	if g, ok := w.(BatchGuard); ok {
		g.LockBatch()
		return g.UnlockBatch
	}
	key := lockKey(w)
	v, _ := batchLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// lockKey 返回传输在批次锁表中的稳定标识。
func lockKey(w WriteTransport) any {
	return globalBatchKey{}
}

// globalBatchKey 是非 BatchGuard 传输的统一键:共享一把全局锁。
type globalBatchKey struct{}
