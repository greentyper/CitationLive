package main

import (
	"encoding/binary"
	"math"
	"os"
)

func writeNativeTTSWave(samples []float32, rate int) {
	peak := float64(0)
	for _, s := range samples {
		if v := math.Abs(float64(s)); v > peak {
			peak = v
		}
	}
	if peak == 0 {
		return
	}
	b := make([]byte, 44+len(samples)*2)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(len(samples)*2))
	for i, s := range samples {
		v := float64(s) * 26000 / peak
		binary.LittleEndian.PutUint16(b[44+i*2:], uint16(int16(v)))
	}
	os.WriteFile("/tmp/citation-native-ack.wav", b, 0600)
}
