package modbus

import "fmt"

// Point 描述一个经映射的数据点位。
type Point struct {
	Name     string
	Region   Region
	Address  uint16        // 起始地址（寄存器为字地址；线圈即位地址）
	Type     DataType      // 寄存器区标量类型；线圈区忽略
	Order    ByteWordOrder // 字序/字节序排列标签；线圈区与 16 位类型忽略
	BitPoint bool          // true 表示这是寄存器内的位点位
	Bit      int           // 位点位时的位偏移（0-15），仅 BitPoint 为 true 时有效
}

// RegisterCount 返回该点位占用的寄存器数（线圈区为 0）。
func (p Point) RegisterCount() int {
	if !p.Region.IsRegister() {
		return 0
	}
	return p.Type.Size()
}

// Validate 校验点位定义是否自洽（区域/类型/排列/位偏移）。
func (p Point) Validate() error {
	if p.Name == "" {
		return NewError(KindInvalidArgument, "point name is empty")
	}
	if p.Region.IsRegister() {
		n := p.Type.Size()
		if n == 0 {
			return NewError(KindInvalidArgument,
				fmt.Sprintf("point %s: unknown data type", p.Name))
		}
		if int(p.Address)+n > 0x10000 {
			return NewError(KindInvalidArgument,
				fmt.Sprintf("point %s: address range overflows", p.Name))
		}
		if n > 1 {
			perm, err := permutation(p.Order)
			if err != nil {
				return fmt.Errorf("point %s: %w", p.Name, err)
			}
			if len(perm) != 2*n {
				return NewError(KindInvalidArgument,
					fmt.Sprintf("point %s: order %s width != %d bytes", p.Name, p.Order, 2*n))
			}
		}
		if p.BitPoint && (p.Bit < 0 || p.Bit >= 16) {
			return NewError(KindInvalidArgument,
				fmt.Sprintf("point %s: bit offset out of 0..15", p.Name))
		}
	} else if p.BitPoint {
		return NewError(KindInvalidArgument,
			fmt.Sprintf("point %s: bit offset only applies to register points", p.Name))
	}
	return nil
}
