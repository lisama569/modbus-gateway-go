package modbus

// LRC 计算 Modbus ASCII 使用的纵向冗余校验：
// 对所有字节求和（模 256）后取补（取反加一），结果取低 8 位。
func LRC(data []byte) byte {
	sum := 0
	for _, b := range data {
		sum += int(b)
	}
	return byte(-int8(sum & 0xFF))
}

// CheckLRC 校验 ASCII 帧解出的 LRC 字节是否与数据一致。
func CheckLRC(data []byte, lrc byte) bool { return LRC(data) == lrc }
