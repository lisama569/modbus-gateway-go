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
pkg/modbus/
  pdu.go          统一请求/响应模型(PDU 编解码、异常响应识别)
  frame.go        Frame / Codec / Link 抽象与读错误归一化
  codec_tcp.go    MBAP 分帧(事务标识、协议标识、长度字段校验)
  codec_rtu.go    RTU 分帧(CRC16、无长度字段的帧长度推断)
  codec_ascii.go  ASCII 分帧(':' 起始、CRLF 结束、LRC 校验)
  slave.go        内存模拟从站(四区数据)、故障注入、本地回环服务端
  client.go       会话级串行客户端(超时、重试、失效重连)
  safe_write.go   安全写入状态机(明确拒绝不重发、不确定恢复+读回确认)
pkg/mapping/
  types.go        点位 / 数据类型 / 字节序 / 批量结果模型
  decode.go       字序、字节序与数据类型的确定性兼容转换
  encode.go       写入值的反向编码(DecodeValue 的逆过程)
  batch.go        跨区/超容量批量读取的确定性拆分与部分失败汇总
  write.go        批量写入的校验、确定性拆分、安全执行与部分失败汇总
  classify.go     底层写入错误的不确定语义/尝试次数适配
cmd/modbus-gateway/  本地验证示例程序
gateway.go        顶层组装(模拟从站 + 客户端 + 映射点位 + 读写批次)
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
| `KindPartial` | 批量读取/写入中存在失败点位（成功部分仍完整保留） |
| `KindUncertain` | 写请求可能已生效但无法确认最终状态（响应丢失且读回无法定论） |

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

## 控制回路：安全批量写回

写回入口为 `Gateway.WriteItems([]mapping.WriteItem)`（底层
`mapping.WriteBatch` + `modbus.Client.SafeWriteCoils/SafeWriteRegisters`）。

### 可写范围与单项失败

| 数据区 | 是否可写 | 使用功能码 |
|--------|----------|-----------|
| 线圈 Coils (0x05/0x0F 区) | 可写 | 点位类型必须为 `Bool`，下发开关量；批量统一走 FC0F |
| 保持寄存器 Holding (0x06/0x10 区) | 可写 | 按点位既有类型与字节序，反向写入 16/32/64 位整数或浮点；批量走 FC10 |
| 离散输入 Discrete Inputs | **只读** | 写入按单项失败（`plan`），不上线 |
| 输入寄存器 Input Registers | **只读** | 同上 |

以下情况一律作为**明确的单项失败**返回，绝不静默跳过、绝不写到别的点位：

- 只读区域点位；线圈点不是 `Bool` / 寄存器点是 `Bool`；
- Go 值类型与点位数据类型不严格匹配（`int16/uint16/int32/...` 宽度不混用、
  `float32` 不接受 `float64` 字面量、NaN/Inf 拒绝），错误 Kind 为 `value`；
- 地址 + 宽度越过 0..65535；
- **同一批次内地址区间重叠**（含一个宽点覆盖另一点）：冲突双方都判 `plan` 失败，
  避免"后写覆盖先写"的不确定结果；
- 底层写入被对端明确拒绝或参数非法（Kind 为 `transport`）；
- 恢复确认用尽仍无法判定（Kind 为 `uncertain`，表示**值可能已生效**）。

### 类型与字节序的反向转换

`mapping.EncodeValue` 是 `DecodeValue` 的严格逆过程：归一化值先按大端展开，
再按与解码相同的自逆字节/字序置换映射回"按地址先后"的寄存器序列。支持的线路
布局与读取完全对称：

- uint16/int16 固定大端（单寄存器，忽略字节序）；
- uint32/int32/float32 支持 ABCD / DCBA / CDAB / BADC；
- uint64/int64/float64 占 4 个寄存器，同样四种字节序；
- 编码后再解码必得到原值（`Encode ∘ Decode` 往返单测覆盖全部类型 × 全部字节序）。

### 写入拆分与部分失败规则

拆分规则与读取**同一套确定性规则**，并额外保证写覆盖范围精确：

1. **跨区域绝不合并**（线圈 FC0F 与寄存器 FC10 地址空间不同）。
2. 组内按起始地址升序（同地址按原始下标稳定排序），区域遍历顺序固定，
   因此同一点位集合的拆分与输入顺序无关。
3. **只合并相邻且连续**的点位：两个点位之间若存在地址间隙，绝不允许一段
   FC0F/FC10 顺带改写任何间隙地址（写与读不同，不能覆盖目标集合以外的单元）。
4. 超过单帧容量（线圈 **1968**、寄存器 **123**）按帧起点贪心拆帧；
   宽点位整体落在一帧或整体进入新帧，**绝不跨帧拆分**。
5. `WriteBatchResult.Results` 与输入**同序一一对应**；某片段失败只令其覆盖的
   点位失败，已成功（已确认）点位的结果完整保留、可见。
6. `Succeeded`/`Failed` 计数明确：`OK=true` 当且仅当全部确认成功；存在失败时
   返回归一化 partial 错误（整体结论可区分"全部成功"与"部分失败"）。
   `SegmentErrors` 与片段一一对应；每个结果还带 `Attempts`（物理写入次数）
   与 `Confirmed`（是否经过读回确认）。

### 恢复与确认语义（安全写入状态机）

`modbus.Client.SafeWriteCoils/SafeWriteRegisters` 对每个片段执行：

1. **对端明确答复不重发**：异常响应（非法地址/数据值/功能码）、本地参数非法
   立刻失败，物理写入恰好一次；这类答复明确，重复下发没有意义。
2. **仅不确定错误才恢复**：超时、连接失效、帧截断/校验/长度/事务标识错误——
   这些情况下"请求可能已生效但响应丢失"。恢复前先**关闭并重建连接**，
   旧连接上的迟到响应随连接丢弃；每次物理写都是新连接上的**新事务标识**，
   因此迟到响应不可能被当作新写的回显，也不会把重复写入混成新结果。
3. **通过读回判断最终状态**：不确定后用 FC01/FC03 读回目标地址区间，与期望值
   逐位/逐寄存器比对——
   - 读回一致：**即使写响应全部丢失，也报成功**（不能把实际成功的控制量报失败）；
   - 读回可靠但不一致：写确实未生效，进入下一轮"重发 + 确认"；
   - 读回本身也不确定（超时/连接失效）：继续下一轮；
   - 读回被明确拒绝（无法再经读回判定）：立即返回 `KindUncertain`；
   - 恢复轮数有上限（`maxWriteConfirmRounds=2`，加上首次至多 3 次物理写），
     用尽后返回 `KindUncertain`——**既不盲报成功也不盲报失败**，上层据此把
     对应点位标记为 `uncertain` 单项失败（值可能已生效）。
4. **并发不串台**：写入与读回确认在同一把会话串行锁内完成，整个
   "写→不确定→重连→重发→读回确认"是一个不可插队的原子会话；并发写各自使用
   独立事务与（恢复时）独立连接，结果严格各归各。已通过 `-race` 并发测试。

`*modbus.Error` 新增分类 `KindUncertain`（"write outcome uncertain"）。

## 模拟从站与故障注入

`modbus.NewSlave(size)` 创建四个数据区的内存从站，可用 `SetHolding/SetInput/
SetCoil/SetDiscrete` 预置数据。`slave.AddInject(&modbus.InjectRule{...})` 支持：

- `Drop`：丢弃请求不响应（复现超时）；丢弃后该连接进入排空状态直到主站关闭，
  保证丢弃帧不会产生迟到响应污染重连后的新事务；
- `ApplyThenDrop`：写请求**先正常落数据再丢弃响应**，精确模拟
  "请求已生效但响应丢失"，用于验证读回确认；
- `Delay`：延迟响应；
- `CorruptChecksum`：破坏 RTU CRC / ASCII LRC；
- `CorruptLength`：破坏 TCP 长度字段（RTU/ASCII 退化为校验破坏）；
- `CorruptTransaction`：破坏 TCP 事务标识；
- `Exception`：返回指定异常码的异常响应；
- `NoApply`：写请求回显成功但**不改动数据区**（"响应成功而值未生效"）；
- `Remaining`：剩余生效次数（-1 永久，1 仅一次，用于"首次失败重试成功"）。

规则按添加顺序匹配，功能码为 0 表示对所有码生效。

## 本地验证方法

```bash
# 运行全部单元/集成测试
# (含畸形报文、校验、超时重试、连接重建、并发,以及写入编码往返、容量拆分、
#  部分失败、明确拒绝不重试、不确定写入恢复/读回确认、并发下发不串台)
go test ./... -count=1

# 带竞态检测的并发验证
go test -race ./... -count=1

# 只看写回相关用例(编码往返/拆分/部分失败 在 mapping,安全写入状态机在 modbus)
go test ./pkg/mapping/ -run 'TestEncode|TestWrite' -v -count=1
go test ./pkg/modbus/ -run TestSafeWrite -v -count=1
go test ./ -run TestEndToEnd.*Write -v -count=1

# 查看单测日志：打印原始报文 raw=... 与归一化判定 normalized=...
go test ./pkg/modbus/ -v -count=1

# 可执行示例：批量读取 + 超时重试恢复 + 安全批量写入 + 响应丢失后读回确认
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

// 安全批量写回:设定值 + 开关量;返回结果与输入同序,部分失败时成功部分保留。
wres, werr := gw.WriteItems([]mapping.WriteItem{
    {Point: points[0], Value: float32(40.0)},
    {Point: points[2], Value: true},
})
```
