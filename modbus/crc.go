package modbus

// CRC16 计算 Modbus RTU 使用的 CRC-16/MODBUS
// （多项式 0xA001 即 0x8005 反向，初始值 0xFFFF，小端序附加到帧尾）。
func CRC16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&0x0001 != 0 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// AppendCRC 在帧尾追加 2 字节 CRC（低字节在前）。
func AppendCRC(frame []byte) []byte {
	crc := CRC16(frame)
	return append(frame, byte(crc), byte(crc>>8))
}

// CheckCRC 校验完整 RTU 帧（末 2 字节为 CRC，低字节在前）。
// 长度不足 3 字节（地址+至少1字节PDU+CRC 边界情况按 2 字节 CRC 处理）返回 false。
func CheckCRC(frame []byte) bool {
	if len(frame) < 2 {
		return false
	}
	body := frame[:len(frame)-2]
	got := uint16(frame[len(frame)-2]) | uint16(frame[len(frame)-1])<<8
	return CRC16(body) == got
}
