# 本地 Modbus 数据接入网关（Go）

用**本地内存模拟从站**替代真实设备，在统一的请求/响应模型之上复现不同传输形态
（Modbus TCP / RTU / ASCII）的报文收发与失败判定。无需串口或真实 PLC 即可
确定性地复现：畸形/截断报文、校验失败、长度不符、事务标识不符、异常响应、
超时重试、连接重建与并发访问。

## 目录结构

```
pkg/modbus/
  pdu.go          统一请求/响应模型(PDU 编解码、异常响应识别)
  frame.go        Frame / Codec / Link 抽象与读错误归一化
  codec_tcp.go    MBAP 分帧(事务标识、协议标识、长度字段校验)
  codec_rtu.go    RTU 分帧(CRC16、无长度字段的帧长度推断)
  codec_ascii.go  ASCII 分帧(':' 起始、CRLF 结束、LRC 校验)
  slave.go        内存模拟从站(四区数据)、故障注入、本地回环服务端
  client.go       会话级串行客户端(超时、重试、失效重连)
pkg/mapping/
  types.go        点位 / 数据类型 / 字节序 / 批量结果模型
  decode.go       字序、字节序与数据类型的确定性兼容转换(读取方向)
  encode.go       解码的严格逆过程:归一化值 -> 寄存器(写回方向)
  batch.go        跨区/超容量批量读取的确定性拆分与部分失败汇总
  write.go        批量写回:可写校验、确定性拆分、批次级隔离、恢复与读回确认
  locker.go       批次级互斥原语(BatchGuard),保证"下发—确认"整体不可插入
cmd/modbus-gateway/  本地验证示例程序
gateway.go        顶层组装(模拟从站 + 客户端 + 映射点位)
```

## 支持的传输形态

| 形态 | 帧封装 | 边界判定 | 校验/一致性 |
|------|--------|----------|-------------|
| TCP  | MBAP(事务ID/协议ID/长度/单元ID) + PDU | MBAP 长度字段 | 协议ID=0、长度、单元ID、事务ID 逐一核对 |
| RTU  | 地址 + PDU + CRC16(低字节在前) | 按功能码与字节计数**确定性推断帧长** | CRC16（多项式 0xA001，初值 0xFFFF） |
| ASCII | `:` + 十六进制(地址+PDU+LRC) + CRLF | `:` 起始、CRLF 结束 | LRC（二补码和），非法十六进制字符同样判错 |

> 真实串口上 RTU 依赖 3.5 字符静默间隔分帧；本地承载在本地回环流上，
> 改用与功能码绑定的长度推断分帧，语义等价且**完全可复现**。三种形态共用
> 同一套 `Request`/`Response`/`Frame` 模型与同一套错误分类。

## 支持的功能码范围

| 码  | 功能 | 单帧数量上限 |
|-----|------|--------------|
| 0x01 | 读线圈 | 2000 |
| 0x02 | 读离散输入 | 2000 |
| 0x03 | 读保持寄存器 | 125 |
| 0x04 | 读输入寄存器 | 125 |
| 0x05 | 写单个线圈 | 1 |
| 0x06 | 写单个寄存器 | 1 |
| 0x0F | 写多个线圈 | 1968 |
| 0x10 | 写多个寄存器 | 123 |

此外完整识别 Modbus 异常码 0x01–0x0B（非法功能、非法地址、非法数据值、
从站故障、确认、从站忙、奇偶错、网关路径不可达、网关无响应）。

## 统一帧模型与失败判定

上层只面对 `modbus.Request` / `modbus.Response`；`Codec` 负责各形态的分帧与
校验。任何异常都**不会被静默丢弃或当作正常数据返回**，而是归一化为
`*modbus.Error`，通过 `modbus.KindOf(err)` 得到稳定分类：

| ErrorKind | 触发条件 |
|-----------|----------|
| `KindTruncated` | 帧不完整：头部截断、PDU 在连接断开前未读满、ASCII 缺 CRLF/过短 |
| `KindLengthMismatch` | TCP 长度字段与实际载荷不一致（含声明长度大于实际字节）、字节计数与数量不符、协议标识非 0 |
| `KindChecksum` | RTU CRC16 错误；ASCII LRC 错误或含非法十六进制字符 |
| `KindTransactionMismatch` | TCP 事务ID 与本请求不一致；响应/异常功能码与请求不一致；单元标识符不符 |
| `KindUnsupportedFunction` | 功能码不在上表支持范围内 |
| `KindExceptionResponse` | 从站返回异常响应（`*Error.ExceptionCode` 携带异常码） |
| `KindTimeout` | 请求在超时时间内**一个字节都没收到**（对端完全不响应） |
| `KindClosed` | 连接被关闭或拨号失败 |
| `KindAddress` | 起始地址/数量非法（0、超上限、越界、单线圈值非 0x0000/0xFF00） |
| `KindPartial` | 批量读取中存在失败点位（成功部分仍完整保留） |

**超时与长度不符的区分**（关键）：对端完全不响应 → 读超时为 `KindTimeout`；
TCP 声明了长度、也确实收到了部分体字节但在超时/断开前没收满 →
`KindLengthMismatch`。二者原因不同、可稳定区分，测试中有对应用例。

## 会话级并发调度语义

- 整个会话持有**单把串行锁**：同一连接上的请求严格 FIFO，请求与响应一一对应，
  绝不交叉错配。并发安全，已在三种形态下做 `-race` 并发测试。
- TCP 每次物理请求使用**新的事务标识**并严格核对响应；RTU/ASCII 无事务标识，
  依靠串行锁保证配对。
- **超时、校验失败、长度不符、事务标识不符、截断**等一律判定当前连接失效，
  重试前先关闭并**重建连接**。旧连接随之关闭，其迟到响应被丢弃，
  绝不会混入新请求的结果（不会出现结果混淆）。
- **异常响应、地址非法、不支持功能码**属于对端的"明确答复"，**不重试**。
- 重试次数 `Retry` = 首次之外的额外尝试次数；每次重试都是**新连接上的新事务**，
  因此不会在同一连接上重复提交同一请求。Modbus 支持的这些读写码均为
  幂等语义，物理重发不会造成状态歧义。
- 拨号失败同样计入尝试并按相同规则重试；服务端在原地址重启后客户端可自动恢复。

## 点位映射与批量读取

### 兼容转换（字序 / 字节序 / 数据类型）

支持类型：`Bool, Uint16, Int16, Uint32, Int32, Float32, Uint64, Int64, Float64`。

寄存器按地址先后读出，四种字节序采用通用 ABCD 记法（A 为最高字节）：

| ByteOrder | 排列 | 说明 |
|-----------|------|------|
| `BigEndian` | ABCD | 不换字、不换字节（Modbus 标准大端） |
| `LittleEndian` | DCBA | 换字且换字节（标准小端） |
| `BigEndianByteSwap` | CDAB | 换字、字内不换字节（字交换） |
| `LittleEndianByteSwap` | BADC | 不换字、字内换字节（字节交换） |

16 位类型只有一个寄存器，始终按大端解析。转换规则确定、可解释：先按每寄存器
大端展开为线性字节，再按字序/字节序重排后按类型解码。原始寄存器字节也会保留在
`PointResult.Raw` 中便于追溯。

### 拆分规则（确定且与输入顺序无关）

1. **按数据区分组**：线圈 / 离散输入 / 保持寄存器 / 输入寄存器绝不合并
   （功能码与地址空间不同）。
2. 组内按起始地址升序（同地址按原始下标稳定排序）。
3. 以"当前帧首个点位地址"为帧起点**贪心合并**：当且仅当
   `候选点起点 + 宽度 - 帧起点 <= 单帧容量` 才并入；放不下立即另开一帧。
   位区容量 2000，寄存器区容量 125。宽点位（2/4 寄存器）**整体落在一帧或整体
   进入新帧，绝不跨帧拆分**。
4. 帧内间隙随合并被一并读出（不发空洞请求），但不会仅为覆盖间隙扩大拆分。
5. 同一点位集合无论输入顺序如何，拆分结果完全相同。

### 部分失败结论

- 每个片段独立读取；某片段失败只令其覆盖的点位失败，其余片段照常执行。
- `BatchResult.Results` 与输入点位**同序一一对应**，每项独立携带值或错误；
  `SegmentErrors` 与片段一一对应。
- `Succeeded` / `Failed` 计数明确；全部成功时 `OK=true` 且 `error==nil`，
  存在失败时返回归一化的 partial 错误，**已成功部分仍完整保留**。

## 批量写回（控制回路）

在同一份点表之上新增写回能力：线圈下发开关量，保持寄存器按点位既有类型与字节序
**反向编码**下发 16/32/64 位整数或浮点值。入口为 `mapping.WriteBatch`，
网关层对应 `gw.WriteAll(values, cfg)` 与 `gw.WriteRequests(reqs, cfg)`。

### 可写范围与单项失败

| 数据区 | 是否可写 | 下发功能码 |
|--------|----------|-----------|
| 线圈 Coils（0x） | ✅ | 单点 0x05，连续多点 0x0F |
| 保持寄存器 Holding（4x） | ✅ | 单寄存器点 0x06，连续多点 0x10 |
| 离散输入 Discrete Inputs（1x） | ❌ 只读 | 不产生任何下发 |
| 输入寄存器 Input Registers（3x） | ❌ 只读 | 不产生任何下发 |

以下情况都作为**明确的单项失败**返回（`WriteResult.Err`），不会静默跳过，
也绝不会写到别的点位，且不产生任何物理报文：

- 点位落在离散输入 / 输入寄存器等只读区；
- 值的 Go 类型与点位类型不**严格一致**（不做隐式数值转换，例如 `TypeUint32`
  只接受 `uint32`，传 `uint16`/`int`/`nil` 均失败）；
- 位区点位不是 `TypeBool`，或寄存器点位是 `TypeBool`；
- 地址 + 宽度越过 0..65535；
- 任意两个合法点位的写入区间**重叠**（冲突双方都判失败，避免一帧内对同一地址
  给出两个值或漏写其一）。

### 类型与字节序反向转换

`mapping.EncodeValue(t, order, value)` 是 `DecodeValue` 的**严格逆过程**：
同一类型同一字节序，解码后的值再编码得到的寄存器与原线路字节逐位一致，反之亦然
（单测 `TestEncodeDecodeRoundTrip` 覆盖九种类型 × 四种字节序）。值与类型的
对应关系：

`bool→Bool`、`uint16/int16→16位`、`uint32/int32/float32→32位`、
`uint64/int64/float64→64位`。16 位类型固定按 Modbus 标准大端输出（忽略字节序）；
32/64 位按点位 `Order` 决定字序与字内字节序。宽点位占用的多个寄存器作为整体，
绝不被拆到两帧。

### 写回的确定性拆分规则

与读取的关键区别：**不能顺带改写不属于请求的间隙地址**。

1. 跨区绝不合并；组内按地址升序（同地址按原始下标）稳定排序。
2. 仅当候选点位地址与当前帧尾**严格连续**（中间无间隙）且并入后不超过单帧容量时
   才合并；有间隙立即另开一帧 —— 间隙地址保持不动。
3. 单帧容量：线圈 1968（0x0F），寄存器 123（0x10）。宽点位整体进帧，不跨帧。
4. 单点片段用单写功能码（0x05 / 0x06），多点连续片段用多写（0x0F / 0x10）。
5. 同一点位集合的拆分结果与输入顺序无关，完全确定。

### 结果规则（同序、保留成功项）

- `WriteBatchResult.Results` 与输入请求**同序一一对应**，每项携带值、
  物理下发次数 `Attempts`、是否经读回确认 `Confirmed`、**确定程度 `Status`** 与错误；
  `Segments` / `SegmentErrors` 与片段一一对应。
- 某片段失败只令其覆盖点位失败，**已成功片段/点位完整保留**。
- 全部成功 `OK=true, error==nil`；存在失败返回归一化 partial 错误，
  `Succeeded`/`Failed` 明确，且 `Failed == Rejected + Uncertain`，
  可区分"全部成功"与"部分失败"。

### 结果的三种确定程度（控制回路必须区分）

每个点位的 `WriteResult.Status` 取以下三值之一，`PossibleApplied()` 是
便于回路判读的快捷结论（"该控制量是否可能已在设备上生效"）：

| Status | 含义 | PossibleApplied | 恢复/重发规则 |
|--------|------|-----------------|---------------|
| `StatusOK` | **已确认生效**：对端明确应答成功；或响应丢失后读回比对一致（此时 `Confirmed=true`） | true | 不再下发 |
| `StatusRejected` | **确定没有生效**：静态校验失败（只读区/类型与宽度不符/地址重叠，无任何下发）或对端明确拒绝（异常响应 0x01–0x0B、非法地址/数据值、不支持功能码） | false | **绝不重发**，只下发一次，调用方不应再写 |
| `StatusUncertain` | **可能已生效但最终未确认**：超时、连接失效、帧截断、校验/长度/事务标识错误等"响应可能丢失"类失败，已在 `MaxAttempts`（默认 3）内恢复仍无法确认 | true | 已在允许次数内处理完毕，**不会无休止重发**；设备真实值可能正是目标值，需上层/人工按"可能已生效"处置，而不是当作没写过 |

即：**确定被拒绝的不会再发；不确定的只在 `MaxAttempts` 次以内处理**，
且重复写入不会掺进成功结论——读回比对一致就立即停止下发并判成功。
对应错误的 `MapError.Kind` 分别为 `rejected` 与 `uncertain`。

### 并发下发的顺序与隔离保证

- 底层会话仍是**单请求串行锁**：同一连接上的请求严格 FIFO、一一配对，
  但它只能保证单次请求-响应不交叉。
- 批量写回在其上再加一层**批次级互斥**（`*modbus.Client` 实现
  `mapping.BatchGuard`，复用独立的批次锁；其他传输由映射层按传输加锁）：
  **一个批次完整的"下发—读回确认—恢复重发"循环是不可被其他批次插入的整体**。
  因此同一传输上多批并发写同一片区域、同一批点位时：
  - 本批的读回比对到的**只能是本批写下的值**，不会读到别的批次的中间态，
    也就不会把别的批次的值误当成本批的确认结果；
  - 本批的恢复重发**不会覆盖别的批次刚刚写成功的控制量**；
  - 批次严格串行、顺序确定，每个批次返回的成功/失败与其自身写入一致，
    最终设备状态与最后一个成功批次报告的值一致（不会出现"双方都报成功、
    设备最终值却对不上"的串台）。
- 规划阶段（只读区/类型/宽度/重叠校验）不产生任何报文，也在批次锁之外完成；
  只有物理下发、读回与重发在锁内。

### 恢复与确认语义（安全优先）

写操作区分两类失败，恢复策略不同：

- **明确拒绝 / 参数非法，绝不重发**：异常响应（0x01–0x0B）、非法地址/数据值、
  不支持功能码等属于对端的确定答复，立即判该片段失败（只下发一次）。
  这类判定复用 `*modbus.Error` 的错误分类（`Error.Retryable()`）。
- **不确定失败才允许恢复**：超时、连接失效、帧截断、校验/长度/事务标识错误 ——
  这些情况下请求**可能已经生效**。此时不盲目重发，而是先**读回**该片段覆盖区间，
  与期望载荷逐位/逐寄存器比对：
  - 读回一致 → 判**成功**，`Status=StatusOK`、`Confirmed=true`，且**不重复下发**
    （"响应丢失但实际写成功"不会被误报失败）；
  - 读回不一致或读回本身失败 → 在 `WriteConfig.MaxAttempts`（默认 3）内**重新下发**；
    0x05/0x06/0x0F/0x10 都是**绝对值写入、幂等**，重发不会产生叠加或歧义；
  - 次数耗尽后再做一次读回，仍无法确认才判失败，但状态明确标为
    **`StatusUncertain`（可能已生效但未确认）** —— 绝不把没确认生效的写入
    报成成功，也不与"确定没生效"混为一谈，更不会超过 `MaxAttempts` 无休止重发。
  - 读回确认默认开启（`WriteConfig.Confirm` 取 `ConfirmDefault`/`ConfirmOn`）；
    显式传 `ConfirmOff` 可关闭，此时不确定失败直接在次数内重发、耗尽同样标
    `StatusUncertain`。
- 对端明确拒绝/静态非法的点位结果为 `StatusRejected`，`PossibleApplied()==false`，
  与上面的 `StatusUncertain`（`PossibleApplied()==true`）在结果字段上即可直接区分。
- 底层客户端在每次物理尝试上仍保持原有保证：串行锁 FIFO、新事务标识、
  失败即重建连接、旧连接迟到响应随关闭丢弃，因此读回确认与重发都不会把
  迟到响应或重复写入混成新结果。批次级隔离见上一节"并发下发的顺序与隔离保证"。

## 模拟从站与故障注入

`modbus.NewSlave(size)` 创建四个数据区的内存从站，可用 `SetHolding/SetInput/
SetCoil/SetDiscrete` 预置数据。`slave.AddInject(&modbus.InjectRule{...})` 支持：

- `Drop`：丢弃请求不响应（复现超时）；
- `ApplyBeforeDrop`：与 `Drop` 配合，先把写请求正常落地到数据区再丢弃响应，
  复现"请求已生效但响应丢失"的不确定场景，用于验证读回确认；
- `Delay`：延迟响应；
- `CorruptChecksum`：破坏 RTU CRC / ASCII LRC；
- `CorruptLength`：破坏 TCP 长度字段（RTU/ASCII 退化为校验破坏）；
- `CorruptTransaction`：破坏 TCP 事务标识；
- `Exception`：返回指定异常码的异常响应；
- `Remaining`：剩余生效次数（-1 永久，1 仅一次，用于"首次失败重试成功"）。

规则按添加顺序匹配，功能码为 0 表示对所有码生效。

## 本地验证方法

```bash
# 运行全部单元/集成测试
# （含畸形报文、校验、超时重试、连接重建、并发；以及写回往返转换、容量拆分、
#   部分失败、明确拒绝不重试、不确定写入读回确认、并发下发不串台）
go test ./... -count=1

# 带竞态检测的并发验证
go test -race ./... -count=1

# 只跑写回相关用例（mapping 层与端到端）
go test ./pkg/mapping/ -run 'TestEncode|TestPlanWrite|TestWriteBatch' -v -count=1
go test . -run TestEndToEndWrite -v -count=1

# 本次修复重点关注的三条行为（可单独复跑）：
# 1) 多批并发写同一区域的批次隔离/顺序与最终状态一致性
go test ./pkg/mapping/ -run TestWriteBatchSameRegion -v -count=20
go test . -run TestEndToEndConcurrentSameRegionIsolation -v -race -count=5
# 2) 响应丢失且可能已生效：StatusUncertain 标注与 MaxAttempts 重发边界
go test ./pkg/mapping/ -run 'TestWriteBatchUncertain' -v -count=1
go test . -run TestEndToEndWriteUncertainStatusDistinctFromReject -v -count=1
# 3) 明确拒绝不重发：StatusRejected 且只下发一次
go test ./pkg/mapping/ -run 'TestWriteBatchRejected|TestWriteBatchDefiniteReject' -v -count=1
go test . -run TestEndToEndWriteExceptionNotRetried -v -count=1

# 查看单测日志：打印原始报文 raw=... 与归一化判定 normalized=...
go test ./pkg/modbus/ -v -count=1

# 可执行示例：批量读取 + 丢弃后重试恢复 + 批量写回 + 响应丢失读回确认
go run ./cmd/modbus-gateway -transport tcp
go run ./cmd/modbus-gateway -transport rtu
go run ./cmd/modbus-gateway -transport ascii
```

测试日志示例（原始报文与判定结果成对出现）：

```
tx attempt=0 transport=tcp fc=0x03 txn=4 raw=000400000006010300000004
rx attempt=0 normalized=modbus: deadline exceeded while reading frame kind=7 raw=
tx attempt=1 transport=tcp fc=0x03 txn=5 raw=000500000006010300000004
rx attempt=1 normalized=OK fc=0x03 registers=4 bits=0
```

## 最小使用示例

```go
gw, _ := gateway.Start(gateway.Config{
    Transport: modbus.TransportTCP, Unit: 1,
    Timeout: 500 * time.Millisecond, Retry: 1,
    Points: []mapping.Point{
        {Name: "temp", Area: mapping.AreaHoldingRegisters, Address: 0,
            Type: mapping.TypeFloat32, Order: mapping.BigEndian},
    },
}, nil)
defer gw.Close()

gw.Slave().SetHolding(0, 0x4212)
gw.Slave().SetHolding(1, 0x6666) // float32 36.6
res, err := gw.ReadAll()

// 批量写回:值类型必须与点位类型严格一致
wres, werr := gw.WriteAll(map[string]any{
    "temp": float32(37.2),
}, mapping.WriteConfig{MaxAttempts: 3})
// wres.Results 与点位同序;Attempts 为下发次数,Confirmed 表示经读回确认。
// Status 区分三种结论,控制回路据此决定动作:
for _, r := range wres.Results {
    switch r.Status {
    case mapping.StatusOK:                     // 已确认生效(可能 Confirmed=true)
    case mapping.StatusRejected:               // 确定没生效,不要再发
    case mapping.StatusUncertain:              // 可能已生效但未确认,需上层确认
        _ = r.PossibleApplied()                // uncertain/OK 为 true,rejected 为 false
    }
}
```
