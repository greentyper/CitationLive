package main

import (
	"encoding/binary"
	"math"
)

// Generated audio only: no recording, model voice or device data is bundled.
func publicCueTone(frequency float64) []byte {
	const samples = 1920 // 80 ms at 24 kHz
	pcm := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		envelope := math.Sin(math.Pi * float64(i) / samples)
		value := int16(1800 * envelope * math.Sin(2*math.Pi*frequency*float64(i)/24000))
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(value))
	}
	return pcm
}
