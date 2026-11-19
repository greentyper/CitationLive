package main

// Background Google Assistant actions for ecosystem features that have no
// local API (find my phone, Google reminders, Calendar events).
//
// The stock Assistant is invoked with SendTextQuery, but its TTS is muted via
// the permanent googleVoiceGuard or a temporary assistant-tts redirect. Gemini
// remains the only audible assistant. A background-assistant window suppresses
// our wake handler so the stock interaction cannot start a second Gemini turn.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/http2"
)

var backgroundAssistantUntil atomic.Int64

func assistantBackgroundActive() bool {
	return time.Now().UnixNano() < backgroundAssistantUntil.Load()
}

func ecosystemToolDeclarations() []M {
	return []M{
		{
			"name":        "find_phone",
			"description": "Ring the user's phone through the Google ecosystem. Use for «найди телефон», «позвони на телефон», 'find my phone'.",
			"parameters":  M{"type": "OBJECT", "properties": M{}},
		},
		{
			"name":        "create_google_reminder",
			"description": "Create a Google Tasks/Calendar reminder. Call get_time first and pass its `iso`/`local_iso` value as `at` (RFC3339 with offset), or HH:MM with `date`; never append Z to a local time. The reminder text stays in the user's language.",
			"parameters": M{
				"type": "OBJECT",
				"properties": M{
					"text":       M{"type": "STRING", "description": "What to be reminded about, in the user's language."},
					"at":         M{"type": "STRING", "description": "Absolute time: RFC3339, or HH:MM together with date."},
					"date":       M{"type": "STRING", "description": "YYYY-MM-DD when at is HH:MM."},
					"in_seconds": M{"type": "INTEGER", "description": "Relative delay in seconds instead of at."},
					"when":       M{"type": "STRING", "description": "Optional natural-language fallback if an absolute time cannot be formed."},
				},
				"required": []string{"text"},
			},
		},
		{
			"name":        "create_calendar_event",
			"description": "Add an event to Google Calendar. Call get_time first and pass its `iso`/`local_iso` value as `at` (RFC3339 with offset), or HH:MM with `date`; never append Z to a local time. The event title stays in the user's language.",
			"parameters": M{
				"type": "OBJECT",
				"properties": M{
					"title":      M{"type": "STRING", "description": "Event title, in the user's language."},
					"at":         M{"type": "STRING", "description": "Absolute start time: RFC3339, or HH:MM together with date."},
					"date":       M{"type": "STRING", "description": "YYYY-MM-DD when at is HH:MM."},
					"in_seconds": M{"type": "INTEGER", "description": "Relative delay in seconds instead of at."},
					"when":       M{"type": "STRING", "description": "Optional natural-language fallback if an absolute time cannot be formed."},
				},
				"required": []string{"title"},
			},
		},
	}
}

func handleEcosystemTool(ctx context.Context, name string, args M) (M, bool) {
	switch name {
	case "verify_google_voice_match":
		return handleGoogleVoiceMatch(ctx), true
	case "find_phone":
		if !personalAccessOK() {
			return personalAccessRequired(), true
		}
		if stockVoicePending() && !stockVoiceArmed() {
			deadline := time.Now().Add(5 * time.Second)
		wait:
			for time.Now().Before(deadline) && !stockVoiceArmed() {
				select {
				case <-ctx.Done():
					break wait
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
		if stockVoiceArmed() {
			return finishStockFindPhone(ctx), true
		}
		armStockVoiceAfter(1800 * time.Millisecond)
		return M{
			"ok": true, "needs_repeat": true,
			"instruction": "Скажи по-русски: «Повтори, пожалуйста, по-английски: find my phone». Не говори, что телефон уже звонит.",
		}, true
	case "create_google_reminder":
		if !personalAccessOK() {
			return personalAccessRequired(), true
		}
		text, _ := args["text"].(string)
		at, _ := args["at"].(string)
		when, _ := args["when"].(string)
		date, _ := args["date"].(string)
		if strings.TrimSpace(at) == "" {
			at = when
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return M{"ok": false, "error": "empty reminder text"}, true
		}
		timerArgs := M{"text": text, "at": at, "remote_kind": "task"}
		if date != "" {
			timerArgs["date"] = date
		}
		if v, ok := args["in_seconds"]; ok {
			timerArgs["in_seconds"] = v
		}
		res := timerManager.toolSetReminder(timerArgs)
		res["instruction"] = "Подтверди создание, если ok=true. Если source=local или offline, скажи, что напоминание сохранено на колонке. Если source=task и синхронизация успешна, скажи, что оно в Google Tasks."
		return res, true
	case "create_calendar_event":
		if !personalAccessOK() {
			return personalAccessRequired(), true
		}
		title, _ := args["title"].(string)
		at, _ := args["at"].(string)
		when, _ := args["when"].(string)
		date, _ := args["date"].(string)
		if strings.TrimSpace(at) == "" {
			at = when
		}
		title = strings.TrimSpace(title)
		if title == "" || strings.TrimSpace(at) == "" {
			return M{"ok": false, "error": "event title and an absolute time (at) are required"}, true
		}
		timerArgs := M{"text": title, "at": at, "remote_kind": "event"}
		if date != "" {
			timerArgs["date"] = date
		}
		if v, ok := args["in_seconds"]; ok {
			timerArgs["in_seconds"] = v
		}
		res := timerManager.toolSetReminder(timerArgs)
		res["instruction"] = "Подтверди создание, если ok=true. Если source=local или offline, скажи, что событие сохранено на колонке. Если source=calendar и синхронизация успешна, скажи, что оно в Google Calendar."
		return res, true
	default:
		return nil, false
	}
}

// grpcCallResponse sends a unary gRPC request and reads server-streaming
// response frames until a frame with printable text arrives or the context
// expires. It returns the raw 5-byte-framed body accumulated so far.
func grpcCallResponse(method string, data []byte, timeout time.Duration) ([]byte, error) {
	return grpcCallResponseOnSocket("/data/chrome/chirp/libassist_grpc.socket", method, data, timeout)
}

func grpcCallResponseOnSocket(socket, method string, data []byte, timeout time.Duration) ([]byte, error) {
	frame := make([]byte, 5+len(data))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(data)))
	copy(frame[5:], data)
	rpcPath := "/assistant.api.ConversationService/" + method
	if strings.HasPrefix(method, "/") {
		rpcPath = method
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost"+rpcPath, bytes.NewReader(frame))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/grpc")
	req.Header.Set("te", "trailers")
	client := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{Timeout: 1500 * time.Millisecond}).DialContext(ctx, "unix", socket)
		},
	}}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out bytes.Buffer
	for {
		var hdr [5]byte
		if _, err := io.ReadFull(res.Body, hdr[:]); err != nil {
			break
		}
		n := int(binary.BigEndian.Uint32(hdr[1:5]))
		if n < 0 || n > 4<<20 {
			break
		}
		msg := make([]byte, n)
		if _, err := io.ReadFull(res.Body, msg); err != nil {
			break
		}
		out.Write(hdr[:])
		out.Write(msg)
		plain := msg
		if hdr[0] == 1 {
			if gz, err := gunzipBytes(msg); err == nil {
				plain = gz
			}
		}
		if len(extractPrintableStrings([][]byte{plain})) > 0 {
			break
		}
	}
	if out.Len() == 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("empty gRPC response")
	}
	return out.Bytes(), nil
}

func gunzipBytes(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(io.LimitReader(zr, 4<<20))
}

func splitGRPCFrames(body []byte) [][]byte {
	var frames [][]byte
	for len(body) >= 5 {
		n := int(binary.BigEndian.Uint32(body[1:5]))
		if n < 0 || len(body) < 5+n {
			break
		}
		msg := body[5 : 5+n]
		if body[0] == 1 {
			if gz, err := gunzipBytes(msg); err == nil {
				msg = gz
			}
		}
		frames = append(frames, msg)
		body = body[5+n:]
	}
	return frames
}

// extractPrintableStrings finds human-readable UTF-8 runs in protobuf bytes.
// It is intentionally conservative: proto type names, JSON blobs and binary
// noise are filtered out so the model only receives likely dialog text.
func extractPrintableStrings(frames [][]byte) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range frames {
		var run []rune
		flush := func() {
			if len(run) < 3 {
				run = run[:0]
				return
			}
			s := strings.TrimSpace(string(run))
			run = run[:0]
			if len([]rune(s)) < 3 || !hasLetter(s) {
				return
			}
			if strings.Contains(s, "type.googleapis.com/") || strings.Contains(s, "assistant.api.") {
				return
			}
			if seen[s] {
				return
			}
			seen[s] = true
			out = append(out, s)
		}
		for i := 0; i < len(b); {
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size == 1 {
				flush()
				i++
				continue
			}
			if unicode.IsPrint(r) && (unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsPunct(r) || unicode.IsSpace(r) || unicode.IsSymbol(r)) {
				run = append(run, r)
			} else {
				flush()
			}
			i += size
		}
		flush()
	}
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}

func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

func bestAssistantText(stringsIn []string) string {
	best := ""
	bestScore := -1
	for _, s := range stringsIn {
		letters, spaces := 0, 0
		for _, r := range s {
			if unicode.IsLetter(r) {
				letters++
			}
			if unicode.IsSpace(r) {
				spaces++
			}
		}
		if letters < 3 {
			continue
		}
		score := letters + spaces*2
		if strings.Contains(s, " ") && score > bestScore {
			best, bestScore = s, score
		}
	}
	if best == "" && len(stringsIn) > 0 {
		best = stringsIn[0]
	}
	return best
}

// assistantBackgroundQuery sends a text query to the stock Assistant with TTS
// muted and returns the Assistant's own response text, if the gRPC stream
// exposes it. The caller (Gemini) is responsible for translating/answering in
// the user's language.
func assistantBackgroundQuery(ctx context.Context, query string) M {
	return assistantBackgroundQueryExtra(ctx, query, nil)
}

func assistantBackgroundQueryExtra(ctx context.Context, query string, extra []byte) M {
	query = strings.TrimSpace(query)
	if query == "" {
		return M{"ok": false, "error": "empty assistant query"}
	}
	var redirect *mixerRedirect
	if !googleVoiceGuardActive.Load() {
		r, err := openTTSRedirectFor(false)
		if err != nil {
			return M{"ok": false, "error": "cannot mute stock assistant TTS: " + clean(err)}
		}
		redirect = r
		defer redirect.close()
	}
	backgroundAssistantUntil.Store(time.Now().Add(30 * time.Second).UnixNano())
	defer backgroundAssistantUntil.Store(0)
	resetAssistantTTSMeta()
	resetAssistantPCM()
	log.Printf("Background Assistant query started")
	payload := append(pbMessage(1, []byte(query)), extra...)
	if err := grpcCall("SendTextQuery", payload); err != nil {
		_ = grpcCall("StopInteraction", []byte{8, 1})
		return M{"ok": false, "error": clean(err)}
	}
	// SendTextQuery returns an empty ack. The spoken answer arrives as muted
	// assistant-tts audio through the permanent voice guard. Wait until the
	// audio stops growing, then transcribe it.
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
		if time.Since(start) >= 6*time.Second {
			break
		}
		select {
		case <-ctx.Done():
			break poll
		case <-time.After(100 * time.Millisecond):
		}
	}
	transcript := ""
	transcribeErr := ""
	if len(samples) > 0 && rate > 0 {
		var err error
		transcript, err = transcribeAssistantPCM(ctx, samples, rate)
		if err != nil {
			transcribeErr = clean(err)
		}
	}
	// Give the ecosystem action a moment to commit, then cancel the stock
	// interaction so its follow-up mic cannot wake Gemini.
	select {
	case <-ctx.Done():
	case <-time.After(250 * time.Millisecond):
	}
	_ = grpcCall("StopInteraction", []byte{8, 1})
	log.Printf("Background Assistant query stopped: pcm=%d transcript=%q err=%q", len(samples), transcript, transcribeErr)
	res := M{"ok": true, "assistant_response": transcript, "pcm_samples": len(samples)}
	if transcript != "" {
		res["assistant_strings"] = []string{transcript}
	}
	if transcribeErr != "" {
		res["transcribe_error"] = transcribeErr
	}
	return res
}

// assistantRawQuery is a diagnostic helper that calls any Assistant gRPC
// method with a simple text payload and returns the raw frames plus any
// printable strings. It is exposed only through the local control endpoint.
func assistantRawQuery(ctx context.Context, method, query string) M {
	method = strings.TrimSpace(method)
	if method == "" {
		method = "SendTextQuery"
	}
	var payload []byte
	if strings.TrimSpace(query) != "" {
		payload = pbMessage(1, []byte(query))
	}
	var redirect *mixerRedirect
	if !googleVoiceGuardActive.Load() {
		r, err := openTTSRedirectFor(false)
		if err != nil {
			return M{"ok": false, "error": "cannot mute stock assistant TTS: " + clean(err)}
		}
		redirect = r
		defer redirect.close()
	}
	backgroundAssistantUntil.Store(time.Now().Add(14 * time.Second).UnixNano())
	defer backgroundAssistantUntil.Store(0)
	body, err := grpcCallResponse(method, payload, 9*time.Second)
	if err != nil {
		_ = grpcCall("StopInteraction", []byte{8, 1})
		return M{"ok": false, "error": clean(err)}
	}
	frames := splitGRPCFrames(body)
	texts := extractPrintableStrings(frames)
	raw := make([]string, 0, len(frames))
	for i, f := range frames {
		if i >= 4 {
			break
		}
		h := fmt.Sprintf("%x", f)
		if len(h) > 8192 {
			h = h[:8192]
		}
		raw = append(raw, h)
	}
	_ = grpcCall("StopInteraction", []byte{8, 1})
	return M{"ok": true, "method": method, "frames": len(frames), "assistant_strings": texts, "raw_hex": raw}
}

// startBackgroundAssistantAction is kept for callers that do not need the
// Assistant's text. It runs the query asynchronously.
func startBackgroundAssistantAction(query string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		if result := assistantBackgroundQuery(ctx, query); result["ok"] != true {
			log.Printf("Background Assistant action failed: %v", result["error"])
		}
	}()
}

// assistantBackgroundAction sends a text query to the stock Assistant with its
// TTS muted, waits for the ecosystem action to execute, then stops the stock
// interaction. It never lets the stock Assistant speak to the user.
func assistantBackgroundAction(ctx context.Context, query string) error {
	query = strings.TrimSpace(query)
	if query == "" {
		return fmt.Errorf("empty assistant query")
	}
	var redirect *mixerRedirect
	if !googleVoiceGuardActive.Load() {
		r, err := openTTSRedirectFor(false)
		if err != nil {
			return fmt.Errorf("cannot mute stock assistant TTS: %w", err)
		}
		redirect = r
		defer redirect.close()
	}
	backgroundAssistantUntil.Store(time.Now().Add(10 * time.Second).UnixNano())
	defer backgroundAssistantUntil.Store(0)
	log.Printf("Background Assistant action started")
	if err := grpcCall("SendTextQuery", pbMessage(1, []byte(query))); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
	}
	_ = grpcCall("StopInteraction", []byte{8, 1})
	log.Printf("Background Assistant action stopped")
	return nil
}
