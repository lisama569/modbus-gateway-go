// Package mapping 提供 Modbus 点位映射、数据类型兼容转换与批量读取拆分。
package mapping

// Area 是 Modbus 数据区。
type Area int

const (
	AreaCoils Area = iota
	AreaDiscreteInputs
	AreaHoldingRegisters
	AreaInputRegisters
)

// DataType 是点位数据类型。
type DataType int

const (
	TypeBool DataType = iota
	TypeUint16
	TypeInt16
	TypeUint32
	TypeInt32
	TypeFloat32
	TypeUint64
	TypeInt64
	TypeFloat64
)

// ByteOrder 描述 32/64 位多寄存器值的字节序与字序兼容策略。
// Hi/Lo 指字节在寄存器内的高低;HighWord/LowWord 指 16 位字的先后。
type ByteOrder int

const (
	// BigEndian 标准大端:高字在前,字内高字节在前(ABCD)
	BigEndian ByteOrder = iota
	// LittleEndian 标准小端:低字在前,字内低字节在前(DCBA)
	LittleEndian
	// BigEndianByteSwap 高字在前但字节交换(BADC)
	BigEndianByteSwap
	// LittleEndianByteSwap 低字在前但字节交换(CDAB)
	LittleEndianByteSwap
)

// Point 是一个映射点位。
type Point struct {
	Name    string
	Area    Area
	Address uint16
	Type    DataType
	Order   ByteOrder // 仅对多寄存器数值类型有意义
	// Scale 与 Offset 仅用于文档/展示,原始解码不应用缩放。
	Scale  float64
	Offset float64
}

// Segment 是按确定规则拆分后的一次物理读取。
type Segment struct {
	Area     Area
	Address  uint16
	Quantity uint16
	Points   []int // 覆盖的点位下标(含被顺带读出的间隙点位之外的目标点)
}

// PointResult 是单个点位的读取结果。
type PointResult struct {
	Index int
	Point Point
	// Raw 是该点占用的原始寄存器字节(大端拼接),Bool 点位使用 BoolValue。
	Raw []byte
	// Value 是归一化解码值:bool/int64/uint64/float64 之一
	Value any
	Err   error
}

// BatchResult 是一次批量读取的整体结论。
type BatchResult struct {
	// OK 为 true 当且仅当所有点位均成功。
	OK bool
	// Results 与输入点位一一对应(同序)。
	Results []PointResult
	// Succeeded 成功解码的点位数。
	Succeeded int
	// Failed 失败的点位数。
	Failed int
	// SegmentErrors 与拆分片段一一对应的错误(nil 表示该片段成功)。
	SegmentErrors []error
	// Segments 拆分结果。
	Segments []Segment
}

// RegisterReader 是批量读取依赖的底层传输能力。
type RegisterReader interface {
	ReadCoils(addr, qty uint16) ([]bool, error)
	ReadDiscreteInputs(addr, qty uint16) ([]bool, error)
	ReadHoldingRegisters(addr, qty uint16) ([]uint16, error)
	ReadInputRegisters(addr, qty uint16) ([]uint16, error)
}
