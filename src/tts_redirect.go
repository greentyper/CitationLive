package main

import (
	"bytes"
	"encoding/binary"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// stockAssistantTTSMeta captures human-readable metadata from the stock
// Assistant's TTS stream. The permanent googleVoiceGuard already drains that
// stream; recording its metadata lets Gemini read what the Assistant said
// without ever making the stock voice audible.
var stockAssistantTTSMeta struct {
	sync.Mutex
	strings []string
	at      time.Time
}

var assistantTTSAudioSamples atomic.Int64
var assistantTTSMetaEvents atomic.Int64

var stockAssistantPCM struct {
	sync.Mutex
	samples []float32
	rate    int
	at      time.Time
}

func resetAssistantPCM() {
	stockAssistantPCM.Lock()
	stockAssistantPCM.samples = nil
	stockAssistantPCM.rate = 48000
	stockAssistantPCM.at = time.Time{}
	stockAssistantPCM.Unlock()
}

func recordAssistantPCM(audio []byte, rate int) {
	if len(audio) < 8 {
		return
	}
	frames := len(audio) / 8
	stockAssistantPCM.Lock()
	defer stockAssistantPCM.Unlock()
	if stockAssistantPCM.rate == 0 {
		stockAssistantPCM.rate = rate
	}
	// Bound the buffer to 30 seconds of mono 48 kHz audio.
	maxSamples := 30 * 48000
	if len(stockAssistantPCM.samples)+frames > maxSamples {
		return
	}
	for i := 0; i < frames; i++ {
		a := math.Float32frombits(binary.LittleEndian.Uint32(audio[i*4:]))
		b := math.Float32frombits(binary.LittleEndian.Uint32(audio[(frames+i)*4:]))
		stockAssistantPCM.samples = append(stockAssistantPCM.samples, (a+b)/2)
	}
	stockAssistantPCM.at = time.Now()
}

func latestAssistantPCM() ([]float32, int, time.Time) {
	stockAssistantPCM.Lock()
	defer stockAssistantPCM.Unlock()
	return append([]float32(nil), stockAssistantPCM.samples...), stockAssistantPCM.rate, stockAssistantPCM.at
}

// assistantTTSWav returns the most recent muted stock-Assistant TTS as a
// mono 16-bit WAV. It is used only for local diagnostics/transcription.
func assistantTTSWav() []byte {
	samples, rate, _ := latestAssistantPCM()
	return encodeWAV(samples, rate)
}

func encodeWAV(samples []float32, rate int) []byte {
	if len(samples) == 0 || rate <= 0 {
		return nil
	}
	var buf bytes.Buffer
	dataLen := len(samples) * 2
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVEfmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint32(rate))
	binary.Write(&buf, binary.LittleEndian, uint32(rate*2))
	binary.Write(&buf, binary.LittleEndian, uint16(2))
	binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(dataLen))
	for _, v := range samples {
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		binary.Write(&buf, binary.LittleEndian, int16(v*32767))
	}
	return buf.Bytes()
}

func resetAssistantTTSMeta() {
	stockAssistantTTSMeta.Lock()
	stockAssistantTTSMeta.strings = nil
	stockAssistantTTSMeta.at = time.Time{}
	stockAssistantTTSMeta.Unlock()
}

func recordAssistantTTSMeta(texts []string) {
	if len(texts) == 0 {
		return
	}
	assistantTTSMetaEvents.Add(1)
	stockAssistantTTSMeta.Lock()
	defer stockAssistantTTSMeta.Unlock()
	seen := map[string]bool{}
	for _, s := range stockAssistantTTSMeta.strings {
		seen[s] = true
	}
	for _, s := range texts {
		if !seen[s] {
			stockAssistantTTSMeta.strings = append(stockAssistantTTSMeta.strings, s)
			seen[s] = true
		}
	}
	stockAssistantTTSMeta.at = time.Now()
}

func latestAssistantTTSMeta() ([]string, time.Time) {
	stockAssistantTTSMeta.Lock()
	defer stockAssistantTTSMeta.Unlock()
	out := append([]string(nil), stockAssistantTTSMeta.strings...)
	return out, stockAssistantTTSMeta.at
}

// Drain only native assistant TTS through a scoped mixer redirection. Unlike
// StopAllTts this lets the original interaction finish and run media actions.
type mixerRedirect struct {
	conn             net.Conn
	done             chan struct{}
	mu               sync.Mutex
	packets, samples int
	peak             float64
	pcm              []float32
	rate             int
}

func openTTSRedirect() (*mixerRedirect, error) {
	return openTTSRedirectFor(true)
}

type redirectPattern struct {
	content uint64
	device  string
}

func openTTSRedirectFor(includeAllCommunication bool) (*mixerRedirect, error) {
	selected := []redirectPattern{{2, "assistant-tts"}, {0, "assistant-tts"}, {3, "assistant-tts"}}
	if includeAllCommunication {
		selected = append(selected, redirectPattern{2, "*"})
	}
	return openMixerRedirect(selected)
}
func openMixerRedirect(selected []redirectPattern) (*mixerRedirect, error) {
	c, e := mixerDial()
	if e != nil {
		return nil, e
	}
	var request []byte
	for _, v := range [][2]uint64{{1, 0}, {2, 2}, {3, 2}, {4, 1}, {5, 0}} {
		request = append(request, pbInt(int(v[0]), v[1])...)
	}
	var patterns []byte

	for _, p := range selected {
		pattern := append(pbInt(1, p.content), pbMessage(2, []byte(p.device))...)
		patterns = append(patterns, pbMessage(1, pattern)...)
	}
	message := append(pbMessage(11, request), pbMessage(12, patterns)...)
	if e = mixerProto(c, message); e != nil {
		c.Close()
		return nil, e
	}
	r := &mixerRedirect{conn: c, done: make(chan struct{}), rate: 48000}
	go func() {
		defer close(r.done)
		for {
			packet, e := mixerRead(c)
			if e != nil {
				return
			}
			if len(packet.meta) > 0 {
				var texts []string
				for _, v := range packet.meta {
					if len(v.data) > 0 {
						texts = append(texts, extractPrintableStrings([][]byte{v.data})...)
					}
				}
				recordAssistantTTSMeta(texts)
			}
			if len(packet.audio) > 0 {
				assistantTTSAudioSamples.Add(int64(len(packet.audio) / 4))
				recordAssistantPCM(packet.audio, r.rate)
				r.mu.Lock()
				r.packets++
				r.samples += len(packet.audio) / 4
				if cfg.TestTrace && len(r.pcm) < r.rate*12 {
					frames := len(packet.audio) / 8
					for i := 0; i < frames; i++ {
						a := math.Float32frombits(binary.LittleEndian.Uint32(packet.audio[i*4:]))
						b := math.Float32frombits(binary.LittleEndian.Uint32(packet.audio[(frames+i)*4:]))
						r.pcm = append(r.pcm, (a+b)/2)
					}
				}
				for i := 0; i+3 < len(packet.audio); i += 4 {
					v := math.Abs(float64(math.Float32frombits(binary.LittleEndian.Uint32(packet.audio[i:]))))
					if !math.IsNaN(v) && v > r.peak {
						r.peak = v
					}
				}
				r.mu.Unlock()
			}
		}
	}()
	return r, nil
}
func (r *mixerRedirect) close() M {
	r.conn.Close()
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	if cfg.TestTrace && len(r.pcm) > 0 {
		writeNativeTTSWave(r.pcm, r.rate)
	}
	return M{"packets": r.packets, "samples": r.samples, "peak": r.peak}
}
