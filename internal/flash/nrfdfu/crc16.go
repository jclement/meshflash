package nrfdfu

// CRC16 computes the CRC-16/CCITT-FALSE variant used by the Nordic legacy DFU
// bootloader for both HCI frame integrity and the firmware checksum recorded
// in a package's init packet.
//
// This mirrors nordicsemi/dfu/crc16.py from Adafruit_nRF52_nrfutil. The Python
// original runs on unbounded integers and masks only at the end; the shifts
// that overflow 16 bits there feed only bits the next round discards, so plain
// uint16 arithmetic here is equivalent.
func CRC16(data []byte) uint16 {
	return CRC16Seed(data, 0xFFFF)
}

// CRC16Seed is CRC16 with an explicit starting value, for chunked computation.
func CRC16Seed(data []byte, crc uint16) uint16 {
	for _, b := range data {
		crc = (crc >> 8) | (crc << 8)
		crc ^= uint16(b)
		crc ^= (crc & 0x00FF) >> 4
		crc ^= crc << 12
		crc ^= (crc & 0x00FF) << 5
	}
	return crc
}
