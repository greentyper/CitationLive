package main

// PCM submitted for the current/last utterance, also used by find-phone.
// Reset at utterance start, append only submitted audio, retain after endpoint.
// This keeps the command (including pre-roll) instead of subsequent idle noise.
// The local diagnostic WAV endpoint exposes this bounded recording on request.

import (
	"encoding/binary"
	"sync"
)

const voiceQueryMaxBytes = 120 * 16000 * 2 // last utterance, up to 120 s

var voiceQueryBuffer struct {
	sync.Mutex
	data []byte
}

func resetVoiceQueryBuffer() {
	voiceQueryBuffer.Lock()
	voiceQueryBuffer.data = voiceQueryBuffer.data[:0]
	voiceQueryBuffer.Unlock()
}

func appendVoiceQueryFrame(frame []byte) {
	if len(frame) == 0 {
		return
	}
	voiceQueryBuffer.Lock()
	voiceQueryBuffer.data = append(voiceQueryBuffer.data, frame...)
	if len(voiceQueryBuffer.data) > voiceQueryMaxBytes {
		excess := len(voiceQueryBuffer.data) - voiceQueryMaxBytes
		voiceQueryBuffer.data = append(voiceQueryBuffer.data[:0], voiceQueryBuffer.data[excess:]...)
	}
	voiceQueryBuffer.Unlock()
}

func takeVoiceQueryPCM() []byte {
	voiceQueryBuffer.Lock()
	out := append([]byte(nil), voiceQueryBuffer.data...)
	voiceQueryBuffer.data = voiceQueryBuffer.data[:0]
	voiceQueryBuffer.Unlock()
	return out
}

// voiceQueryWAV returns the current mic buffer for diagnostics. It does not
// consume the buffer, so a test turn can be inspected after it ends.
func voiceQueryWAV() []byte {
	voiceQueryBuffer.Lock()
	data := append([]byte(nil), voiceQueryBuffer.data...)
	voiceQueryBuffer.Unlock()
	if len(data) < 2 {
		return nil
	}
	samples := make([]float32, len(data)/2)
	for i := range samples {
		samples[i] = float32(int16(binary.LittleEndian.Uint16(data[i*2:]))) / 32768
	}
	return encodeWAV(samples, 16000)
}
