package main

// Voice query path for the stock Assistant. The recovered protocol is:
//   StartInteraction (near-field mic) -> StreamAudioInput (16 kHz mono PCM,
//   10 ms chunks) -> END_OF_AUDIO -> muted TTS -> StopInteraction.
// No request carries a speaker id; Voice Match is derived by the backend from
// the PCM, so the user's actual voice must be sent.

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
)

const assistantAudioInputSocket = "/data/chrome/chirp/libassist_audio_input.socket"

func startInteractionPayload() []byte {
	// ClientInteraction.field5=true: the client will provide PCM through
	// AudioInputService. This is the stock client-audio mode; a mic_type
	// interaction uses the physical microphone and leaves the audio-input
	// listener without an active stream.
	return []byte{0x0a, 0x02, 0x28, 0x01}
}

var stockVoiceArmedUntil atomic.Int64
var stockVoicePendingUntil atomic.Int64

func stockVoiceArmed() bool {
	return time.Now().UnixNano() < stockVoiceArmedUntil.Load()
}

func stockVoicePending() bool {
	return time.Now().UnixNano() < stockVoicePendingUntil.Load()
}

// armStockVoiceAfter starts the stock Assistant microphone after Gemini has
// finished asking the user to repeat. The stock Assistant's listening window
// is short, so arming immediately would miss the user's repeat.
func armStockVoiceAfter(delay time.Duration) {
	stockVoicePendingUntil.Store(time.Now().Add(delay + 15*time.Second).UnixNano())
	go func() {
		time.Sleep(delay)
		if err := startStockVoiceInteraction(); err != nil {
			log.Printf("stock voice arm failed: %v", clean(err))
		}
	}()
}

// armStockVoiceAtWake opens the stock Assistant's microphone immediately after
// the hotword, in parallel with Gemini. The stock Assistant then hears the
// user's initial request and can answer that same voice turn; no repeat is
// needed for its cloud Voice Match decision.
func armStockVoiceAtWake() {
	time.Sleep(50 * time.Millisecond)
	if err := startStockVoiceInteraction(); err != nil {
		log.Printf("stock voice wake arm failed: %v", clean(err))
	}
}

// startStockVoiceInteraction opens the stock Assistant's physical microphone
// with the recovered PresetParam{preset_op=1, name="voice"} payload. The stock
// Assistant then listens for the user's repeat; its TTS remains muted.
func startStockVoiceInteraction() error {
	preset := append(pbInt(1, 1), pbMessage(2, []byte("voice"))...)
	payload := pbMessage(1, pbMessage(4, preset))
	// Suppress our wake handler before the stock Assistant publishes
	// assistant-listening; otherwise watch() starts Gemini and cancels it.
	backgroundAssistantUntil.Store(time.Now().Add(25 * time.Second).UnixNano())
	if err := grpcCall("StartInteraction", payload); err != nil {
		backgroundAssistantUntil.Store(0)
		return err
	}
	resetAssistantPCM()
	stockVoiceArmedUntil.Store(time.Now().Add(20 * time.Second).UnixNano())
	return nil
}

func int16ToFloat32PCM(pcm []byte) []byte {
	if len(pcm)%2 != 0 {
		pcm = pcm[:len(pcm)-1]
	}
	out := make([]byte, len(pcm)*2)
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int16(binary.LittleEndian.Uint16(pcm[i:]))
		f := float32(v) / 32768.0
		binary.LittleEndian.PutUint32(out[i*2:], math.Float32bits(f))
	}
	return out
}

// streamAudioInput sends PCM as a paced client-streaming gRPC request. Pacing
// is required: the stock receiver expects real-time 10 ms blocks and may close
// the stream if the whole utterance arrives at once.
func streamAudioInput(ctx context.Context, pcm []byte, sampleRate, channels int, formatWire int, pace time.Duration) error {
	bytesPerSample := 4
	if formatWire == 1 {
		bytesPerSample = 2
	}
	pr, pw := io.Pipe()
	streamCtx, streamCancel := context.WithTimeout(ctx, 12*time.Second)
	defer streamCancel()
	req, err := http.NewRequestWithContext(streamCtx, "POST",
		"http://localhost/assistant.api.AudioInputService/StreamAudioInput", pr)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/grpc")
	req.Header.Set("te", "trailers")
	client := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{Timeout: 1500 * time.Millisecond}).DialContext(ctx, "unix", assistantAudioInputSocket)
		},
	}}
	writeErr := make(chan error, 1)
	go func() {
		defer pw.Close()
		writeFrame := func(msg []byte) error {
			hdr := make([]byte, 5)
			binary.BigEndian.PutUint32(hdr[1:], uint32(len(msg)))
			if _, err := pw.Write(hdr); err != nil {
				return err
			}
			_, err := pw.Write(msg)
			return err
		}
		base := time.Now().UnixMilli()
		chunkBytes := 160 * channels * bytesPerSample
		first := true
		for off := 0; off < len(pcm); off += chunkBytes {
			end := off + chunkBytes
			if end > len(pcm) {
				end = len(pcm)
			}
			frames := (end - off) / (channels * bytesPerSample)
			if frames <= 0 {
				break
			}
			audioBuf := pbMessage(1, pcm[off:end])
			audioBuf = append(audioBuf, pbInt(2, uint64(base+int64(off/chunkBytes)*10))...)
			audioBuf = append(audioBuf, pbInt(3, uint64(frames))...)
			data := pbMessage(3, audioBuf)
			if first {
				format := append(pbInt(1, uint64(sampleRate)), pbInt(2, uint64(formatWire))...)
				format = append(format, pbInt(3, uint64(channels))...)
				data = append(pbMessage(1, format), data...)
				first = false
			}
			msg := append(pbInt(4, 1), pbMessage(5, data)...)
			if err := writeFrame(msg); err != nil {
				writeErr <- err
				return
			}
			if pace > 0 {
				select {
				case <-ctx.Done():
					writeErr <- ctx.Err()
					return
				case <-time.After(pace):
				}
			}
		}
		if err := writeFrame(pbInt(4, 2)); err != nil {
			writeErr <- err
			return
		}
		writeErr <- nil
	}()
	res, err := client.Do(req)
	if err != nil {
		// The stock AudioInputService keeps the client-streaming response open
		// after END_OF_AUDIO on some firmware builds. If the paced writer has
		// already delivered every frame and the half-close, treat the deadline
		// as a successful upload and let waitForAssistantTTS observe the reply.
		select {
		case werr := <-writeErr:
			if werr == nil && errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
		case <-time.After(2 * time.Second):
		}
		_ = pw.CloseWithError(err)
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	werr := <-writeErr
	status := res.Trailer.Get("grpc-status")
	if status == "" {
		status = res.Header.Get("grpc-status")
	}
	if status != "" && status != "0" {
		return fmt.Errorf("audio input grpc status %s", status)
	}
	return werr
}

func waitForAssistantTTS(ctx context.Context, timeout time.Duration) (string, int, error) {
	var samples []float32
	var rate int
	lastCount := -1
	lastChange := time.Now()
	start := time.Now()
poll:
	for {
		samples, rate, _ = latestAssistantPCM()
		if len(samples) != lastCount {
			lastCount = len(samples)
			lastChange = time.Now()
		}
		if len(samples) >= 12000 && time.Since(lastChange) >= 500*time.Millisecond {
			break
		}
		if time.Since(start) >= timeout {
			break
		}
		select {
		case <-ctx.Done():
			break poll
		case <-time.After(100 * time.Millisecond):
		}
	}
	if len(samples) == 0 || rate <= 0 {
		return "", 0, fmt.Errorf("no assistant TTS captured")
	}
	transcript, err := transcribeAssistantPCM(ctx, samples, rate)
	return transcript, len(samples), err
}

// assistantVoiceQuery sends the user's PCM to the stock Assistant, waits for
// its muted TTS and returns the transcript. The caller must have already
// passed the local Voice Match gate.
func assistantVoiceQuery(ctx context.Context, int16PCM []byte, textAfter string) M {
	if len(int16PCM) < 3200 {
		return M{"ok": false, "error": "voice query audio too short"}
	}
	backgroundAssistantUntil.Store(time.Now().Add(40 * time.Second).UnixNano())
	defer backgroundAssistantUntil.Store(0)
	resetAssistantPCM()
	if err := grpcCall("StartInteraction", startInteractionPayload()); err != nil {
		return M{"ok": false, "error": clean(err)}
	}
	// The audio-input service starts listening asynchronously after the
	// interaction is created.
	select {
	case <-ctx.Done():
	case <-time.After(400 * time.Millisecond):
	}
	floatPCM := int16ToFloat32PCM(int16PCM)
	err := streamAudioInput(ctx, floatPCM, 16000, 1, 3, 10*time.Millisecond)
	if err != nil {
		// A freshly created interaction may need a second dial attempt.
		if strings.Contains(strings.ToLower(clean(err)), "connection refused") {
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
			err = streamAudioInput(ctx, floatPCM, 16000, 1, 3, 10*time.Millisecond)
		}
	}
	if err != nil {
		// Some firmware builds expose the same receiver as signed 16-bit.
		_ = grpcCall("StopInteraction", []byte{8, 1})
		time.Sleep(250 * time.Millisecond)
		_ = grpcCall("StartInteraction", startInteractionPayload())
		err = streamAudioInput(ctx, int16PCM, 16000, 1, 1, 10*time.Millisecond)
	}
	if err != nil {
		_ = grpcCall("StopInteraction", []byte{8, 1})
		return M{"ok": false, "error": clean(err)}
	}
	if textAfter != "" {
		select {
		case <-ctx.Done():
		case <-time.After(600 * time.Millisecond):
		}
		_ = grpcCall("SendTextQuery", pbMessage(1, []byte(textAfter)))
	}
	transcript, sampleCount, err := waitForAssistantTTS(ctx, 9*time.Second)
	_ = grpcCall("StopInteraction", []byte{8, 1})
	res := M{"ok": true, "assistant_response": transcript, "pcm_samples": sampleCount, "voice_query": true}
	if err != nil {
		res["transcribe_error"] = clean(err)
	}
	if transcript != "" {
		res["assistant_strings"] = []string{transcript}
	}
	return res
}

// handleGoogleVoiceMatch starts a deferred verification using the user's
// already-captured request PCM. The stock Assistant is not started during the
// live Gemini session: it would take over the microphone and break the turn.
// Gemini answers "проверяю голос", the deferred worker stops the Live session,
// lets the stock Assistant answer the same voice request, and speaks its
// VM-gated answer. No repeat, no fixed phrase, no name.
var voiceVerifyPending atomic.Bool

func handleGoogleVoiceMatch(ctx context.Context) M {
	if voiceVerifyPending.Load() {
		return M{
			"ok": true, "pending": true, "google_voice_match": false,
			"instruction": "Скажи по-русски: «Проверяю голос». Больше ничего не добавляй.",
		}
	}
	pcm := takeVoiceQueryPCM()
	if len(pcm) < 3200 {
		return M{
			"ok": false, "google_voice_match": false,
			"error":       "voice request audio is empty",
			"instruction": "Скажи по-русски, что не расслышал запрос, и попроси повторить его один раз.",
		}
	}
	voiceVerifyPending.Store(true)
	go verifyGoogleVoiceDeferred(pcm)
	return M{
		"ok": true, "pending": true, "google_voice_match": false,
		"instruction": "Скажи по-русски: «Проверяю голос». Больше ничего не добавляй; ответ придёт отдельно.",
	}
}

func voiceMatchNotVerified(transcript string) bool {
	lower := strings.ToLower(transcript)
	return strings.Contains(lower, "verify your voice") || strings.Contains(lower, "voice match") ||
		strings.Contains(lower, "can't verify") || strings.Contains(lower, "couldn't verify") ||
		strings.Contains(lower, "not sure who") || strings.Contains(lower, "don't know your name") ||
		strings.Contains(lower, "don't know who you are") || strings.Contains(lower, "told me your name") ||
		strings.Contains(lower, "personal results") || strings.Contains(lower, "don't understand") ||
		strings.Contains(lower, "didn't catch") || strings.Contains(lower, "could you repeat") ||
		strings.Contains(lower, "can't help with that") || strings.Contains(lower, "sorry")
}

// verifyGoogleVoiceDeferred runs after the Gemini turn that said "проверяю
// голос". It stops the Live session first, so the stock Assistant's
// client-audio interaction cannot collide with the Gemini websocket.
func verifyGoogleVoiceDeferred(pcm []byte) {
	defer voiceVerifyPending.Store(false)
	time.Sleep(2500 * time.Millisecond)
	ctl.stop()
	ctl.waitForIdle(4 * time.Second)
	_ = grpcCall("StopAllTts", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := assistantVoiceQuery(ctx, pcm, "")
	transcript, _ := res["assistant_response"].(string)
	log.Printf("Google Voice Match deferred answer: ok=%v samples=%v chars=%d transcript=%q", res["ok"], res["pcm_samples"], len(transcript), transcript)
	if res["ok"] == true && strings.TrimSpace(transcript) != "" && !voiceMatchNotVerified(transcript) {
		unlockGoogleVoiceAccess()
		_ = speakGeminiTTS(ctx, "Google подтвердил голос. "+transcript)
		return
	}
	_ = speakGeminiTTS(ctx, "Google не подтвердил голос, поэтому я не могу получить доступ к личным данным.")
}

// assistantTextQuery sends a text query into the already-listening stock
// Assistant session and returns its muted-TTS transcript. It must not start a
// second client-audio interaction: the physical-microphone session is what
// carries the speaker's Voice Match.
func assistantTextQuery(ctx context.Context, text string) M {
	backgroundAssistantUntil.Store(time.Now().Add(20 * time.Second).UnixNano())
	defer backgroundAssistantUntil.Store(0)
	// The repeated request may already have produced a stock-Assistant answer.
	// Let that answer finish before sending the silent VM query; otherwise the
	// captured PCM is the earlier answer, not the VM-gated response.
	waitForAssistantTTSIdle(ctx, 10*time.Second)
	resetAssistantPCM()
	if err := grpcCall("SendTextQuery", pbMessage(1, []byte(text))); err != nil {
		log.Printf("Google Voice Match SendTextQuery failed: %v", clean(err))
		return M{"ok": false, "error": clean(err)}
	}
	transcript, sampleCount, err := waitForAssistantTTS(ctx, 9*time.Second)
	stockVoiceArmedUntil.Store(0)
	stockVoicePendingUntil.Store(0)
	_ = grpcCall("StopInteraction", []byte{8, 1})
	res := M{"ok": true, "assistant_response": transcript, "pcm_samples": sampleCount, "voice_query": true}
	if err != nil {
		res["transcribe_error"] = clean(err)
		log.Printf("Google Voice Match transcription failed: samples=%d err=%v", sampleCount, clean(err))
	}
	return res
}

// waitForAssistantTTSIdle waits until an already-running stock-Assistant TTS
// answer stops growing, or until the quiet timeout expires. It does not
// transcribe or consume the answer.
func waitForAssistantTTSIdle(ctx context.Context, timeout time.Duration) {
	start := time.Now()
	lastCount := -1
	lastChange := time.Now()
	for {
		samples, _, _ := latestAssistantPCM()
		if len(samples) != lastCount {
			lastCount = len(samples)
			lastChange = time.Now()
		}
		if len(samples) == 0 && time.Since(start) >= 700*time.Millisecond {
			return
		}
		if len(samples) > 0 && time.Since(lastChange) >= 500*time.Millisecond {
			return
		}
		if time.Since(start) >= timeout {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func finishStockFindPhone(ctx context.Context) M {
	transcript, sampleCount, err := waitForAssistantTTS(ctx, 10*time.Second)
	stockVoiceArmedUntil.Store(0)
	stockVoicePendingUntil.Store(0)
	res := M{"ok": err == nil && strings.TrimSpace(transcript) != "", "assistant_response": transcript, "pcm_samples": sampleCount, "voice_query": true}
	if err != nil {
		res["error"] = clean(err)
	}
	lowerReply := strings.ToLower(transcript)
	if res["ok"] != true {
		res["ok"] = false
		res["error"] = "stock Assistant did not confirm the phone ring"
		res["instruction"] = "Скажи по-русски, что не удалось запустить поиск телефона. Не говори, что телефон звонит."
		return res
	}
	if strings.Contains(lowerReply, "verify your voice") || strings.Contains(lowerReply, "voice match") {
		res["ok"] = false
		res["error"] = "stock Assistant requires its own Voice Match for find phone"
		res["instruction"] = "Скажи по-русски, что Google не подтвердил голос для поиска телефона и команда не выполнена. Не говори, что телефон звонит."
		return res
	}
	res["instruction"] = "Скажи по-русски, что звонишь на телефон. Если assistant_response на английском — переведи смысл, не читай его дословно."
	return res
}

func resample24kTo16k(pcm []byte) []byte {
	if len(pcm)%2 != 0 {
		pcm = pcm[:len(pcm)-1]
	}
	in := make([]int16, len(pcm)/2)
	for i := range in {
		in[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
	}
	outSamples := len(in) * 2 / 3
	out := make([]byte, outSamples*2)
	for i := 0; i < outSamples; i++ {
		pos := float64(i) * 1.5
		lo := int(pos)
		hi := lo + 1
		if hi >= len(in) {
			hi = len(in) - 1
		}
		frac := pos - float64(lo)
		v := float64(in[lo])*(1-frac) + float64(in[hi])*frac
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(math.Round(v))))
	}
	return out
}
