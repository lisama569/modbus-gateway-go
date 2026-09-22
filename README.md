# modbus-gateway-go

本地 Modbus 数据接入网关：以本地模拟从站替代真实设备，使不同传输形态下的
请求、响应与失败判定可一致复现。

## 支持的传输形态

| 形态 | 标识 | 帧格式 | 说明 |
|------|------|--------|------|
| Modbus TCP | `TransportTCP` | MBAP 头（事务标识+协议标识+长度+单元标识）+ PDU | 事务标识单调递增并校验 |
| Modbus RTU over TCP | `TransportRTU` | 地址 + PDU + CRC16（小端） | 本地模拟串口链路，按功能码定长解码 |
| Modbus ASCII over TCP | `TransportASCII` | `:` + 十六进制 ASCII（地址+PDU+LRC）+ CRLF | 本地模拟串口链路 |

三种形态共享同一 `Request`/`Response` 模型与同一 PDU 编解码器，
仅帧封装（`Codec`）不同。

## 支持的功能码

| 功能码 | 名称 | 数据区 |
|--------|------|--------|
| 0x01 | Read Coils | 线圈 |
| 0x02 | Read Discrete Inputs | 离散输入 |
| 0x03 | Read Holding Registers | 保持寄存器 |
| 0x04 | Read Input Registers | 输入寄存器 |
| 0x05 | Write Single Coil | 线圈 |
| 0x06 | Write Single Register | 保持寄存器 |
| 0x0F | Write Multiple Coils | 线圈 |
| 0x10 | Write Multiple Registers | 保持寄存器 |


其他功能码在客户端编码侧与从站侧都会被识别为
`KindUnsupportedFunction`（从站回异常码 0x01 非法功能）。

## 错误分类

所有失败统一为 `*modbus.Error`，`Kind` 字段可区分：

| Kind | 含义 | 触发场景 |
|------|------|----------|
| `truncated` | 报文截断 | MBAP 头/PDU/RTU 帧/ASCII 帧不完整 |
| `length_mismatch` | 长度不一致 | MBAP 长度字段、PDU 字节计数与实际不符 |
| `checksum` | 校验失败 | RTU CRC16 / ASCII LRC 不匹配 |
| `transaction_mismatch` | 标识错配 | TCP 事务标识、协议标识、单元标识不匹配 |
| `unsupported_function` | 功能码不支持 | 编解码或从站遇到未知功能码 |
| `modbus_exception` | 异常响应 | 功能码最高位置 1，携带标准异常码 |
| `timeout` | 超时 | 等待响应超过 `Timeout` |
| `connection_closed` | 连接失效 | 读写失败、对端关闭 |
| `invalid_argument` | 参数非法 | 数量越界、类型/排列标签非法等 |
| `partial_failure` | 部分失败 | 批量读取中部分点位失败 |

异常响应绝不作为正常数据返回；`AsError(err)` 可把任意错误归一化后读取分类。

## 会话语义（并发调度 / 超时 / 重试 / 重连）

- 同一 `Session` 上的请求由互斥锁串行化：请求-响应严格成对，不交叉错配；
  TCP 形态额外校验单调递增的事务标识。
- 仅 `timeout` / `connection_closed` / `length_mismatch` 触发“重连 + 重试”
  （最多 `MaxRetry` 次）；异常响应、校验失败、标识错配等是确定结论，不重试，
  避免同一写请求被重复提交。
- 重试在同一个 `Do` 调用内完成，调用方只看到一次最终结论；
  超时后连接上的帧边界状态不可信，一律强制重建连接。
- 连接惰性建立，失效后下次请求自动重建；`Stats()` 暴露
  请求数/重试数/重连数/超时数。

## 点位映射与批量读取

### 数据类型与字序/字节序

寄存器区支持 `uint16/int16/uint32/int32/float32/uint64/int64/float64`。
32/64 位类型用排列标签确定地描述字节序与字序组合（以 0x12345678 为例）：

| 标签 | 线路字节 | 含义 |
|------|----------|------|
| `ABCD` | 12 34 56 78 | 大端、字序正向（标准） |
| `CDAB` | 56 78 12 34 | 字交换 |
| `BADC` | 34 12 78 56 | 字节交换 |
| `DCBA` | 78 56 34 12 | 全小端 |

64 位使用 `ABCDEFGH` / `HGFEDCBA` / `GHEFCDAB` / `BADCFEHG` 等同类标签。
标签必须是对应宽度的字母排列，非法标签在点位校验时即报错。

### 拆分规则（确定且可解释）

`SplitChunks` 对一批点位按以下顺序处理：

1. 按数据区分组——跨越不同区域的点位绝不合并到同一请求；
2. 组内按起始地址升序排序（地址相同按点名排序，结果完全确定）；
3. 顺序合并为连续范围（允许空洞，空洞数据一并读取）；
4. 范围超过单次报文容量时按容量边界切分：
   读位 2000 点/次，读寄存器 125 个/次，写线圈 1968 点/次，写寄存器 123 个/次。

### 部分失败语义

`BatchRead` 逐 Chunk 执行：某 Chunk 失败只把其覆盖的点位标记失败，
已成功点位的值照常保留在 `BatchReadResult.Items` 中；
整体结论 `Overall` 为 `KindPartialFailure`，`FailedCount()` 给出失败数。

## 本地验证

```bash
# 单元 + 集成测试（覆盖畸形/截断报文、校验失败、超时重试、
# 连接重建、并发访问；日志打印原始报文与归一化判定）
go test -v ./...

# 三种传输形态端到端演示
go run ./cmd/demo
```

测试日志示例：

```
[tcp-roundtrip] raw=123400000005110302007B -> verdict=ok err=<nil>
[rtu-bad-crc] raw=110302007B0000 -> verdict=checksum err=modbus: checksum: ...
[exception] raw=8302 -> verdict=modbus_exception exception=illegal_data_address
```

## 目录结构

```
modbus/
  errors.go    错误分类与统一错误类型
  pdu.go       统一请求/响应模型与 PDU 编解码
  adu.go       ADU、Codec 接口、帧传输
  codec_tcp.go / codec_rtu.go / codec_ascii.go  三种传输形态
  crc.go / lrc.go  校验
  slave.go     本地模拟从站（含故障注入钩子）
  session.go   会话调度：串行化、超时、重试、重连
  client.go    高层读写 API（含批量写拆分）
  datatype.go  数据类型与字序/字节序转换
  point.go     点位模型
  batch.go     批量读取、确定性拆分、部分失败汇总
cmd/demo/      本地验证入口
```
