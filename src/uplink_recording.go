package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const uplinkRate = 16000

type uplinkEvent struct {
	Kind         string `json:"kind"`
	SentUS       int64  `json:"sent_us"`
	SampleOffset int    `json:"sample_offset"`
	Samples      int    `json:"samples,omitempty"`
}

type uplinkRecording struct {
	mu             sync.Mutex
	active         bool
	started, until time.Time
	pcm            []byte
	events         []uplinkEvent
	reason         string
}

var outboundRecording uplinkRecording

func (d *uplinkRecording) arm(now time.Time, seconds int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.active = true
	d.started, d.until = now, now.Add(time.Duration(seconds)*time.Second)
	d.pcm, d.events, d.reason = nil, nil, ""
}

func (d *uplinkRecording) expire(now time.Time) {
	if d.active && !now.Before(d.until) {
		d.active = false
		d.reason = "time_limit"
	}
}

func outboundMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// Called under the WebSocket write lock, AFTER a successful WriteJSON. Never
// retains credentials, model responses, or raw/intermediate microphone audio.
// A successful local write is not proof of remote receipt or interpretation.
func (d *uplinkRecording) sent(v any, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expire(now)
	if !d.active {
		return
	}
	input := outboundMap(outboundMap(v)["realtimeInput"])
	if input == nil {
		return
	}
	e := uplinkEvent{SentUS: now.Sub(d.started).Microseconds(), SampleOffset: len(d.pcm) / 2}
	switch {
	case input["activityStart"] != nil:
		e.Kind = "activity_start"
	case input["activityEnd"] != nil:
		e.Kind = "activity_end"
	case input["audio"] != nil:
		a := outboundMap(input["audio"])
		mime, _ := a["mimeType"].(string)
		encoded, _ := a["data"].(string)
		pcm, err := base64.StdEncoding.DecodeString(encoded)
		if mime != "audio/pcm;rate=16000" || err != nil || len(pcm)%2 != 0 {
			d.active = false
			d.reason = "unsupported_audio_payload"
			return
		}
		// Keep entire payloads and never alter/normalize/pad any sample.
		if len(d.pcm)+len(pcm) > 30*uplinkRate*2 {
			d.active = false
			d.reason = "audio_limit"
			return
		}
		e.Kind = "audio"
		e.Samples = len(pcm) / 2
		d.pcm = append(d.pcm, pcm...)
	default:
		return
	}
	if len(d.events) >= 2048 {
		d.active = false
		d.reason = "event_limit"
		return
	}
	d.events = append(d.events, e)
}

func (d *uplinkRecording) stop(clear bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.active = false
	d.reason = "stopped"
	if clear {
		d.pcm = nil
		d.events = nil
		d.started = time.Time{}
		d.reason = ""
	}
}

func (d *uplinkRecording) status(now time.Time) M {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expire(now)
	left := 0
	if d.active {
		left = int(d.until.Sub(now).Seconds()) + 1
	}
	return M{"active": d.active, "remaining_seconds": left, "audio_seconds": float64(len(d.pcm)) / 2 / uplinkRate, "bytes": len(d.pcm), "packets": len(d.events), "reason": d.reason, "sample_rate": uplinkRate, "channels": 1, "local_write_only": true}
}

func pcmUplinkWAV(pcm []byte) []byte {
	wav := make([]byte, 44+len(pcm))
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(36+len(pcm)))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], uplinkRate)
	binary.LittleEndian.PutUint32(wav[28:], uplinkRate*2)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], uint32(len(pcm)))
	copy(wav[44:], pcm)
	return wav
}

func registerUplinkRecordingHandlers() {
	http.HandleFunc("/diagnostics/uplink", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/diagnostics/uplink" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(uplinkRecordingPage))
	})
	http.HandleFunc("/diagnostics/uplink/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(outboundRecording.status(time.Now()))
	})
	for _, action := range []string{"start", "stop", "clear"} {
		action := action
		http.HandleFunc("/diagnostics/uplink/"+action, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				w.WriteHeader(405)
				return
			}
			if !localSpotifyForm(r) {
				w.WriteHeader(403)
				return
			}
			if action == "start" {
				outboundRecording.arm(time.Now(), 30)
			} else {
				outboundRecording.stop(action == "clear")
			}
			w.WriteHeader(204)
		})
	}
	http.HandleFunc("/diagnostics/uplink/audio.wav", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		outboundRecording.mu.Lock()
		outboundRecording.expire(time.Now())
		pcm := append([]byte(nil), outboundRecording.pcm...)
		active := outboundRecording.active
		outboundRecording.mu.Unlock()
		if active {
			http.Error(w, "Сначала останови запись", 409)
			return
		}
		if len(pcm) == 0 {
			http.Error(w, "Отправленных аудиопакетов пока нет", 404)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "gemini-uplink.wav", time.Time{}, strings.NewReader(string(pcmUplinkWAV(pcm))))
	})
	http.HandleFunc("/diagnostics/uplink/timeline.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		outboundRecording.mu.Lock()
		defer outboundRecording.mu.Unlock()
		outboundRecording.expire(time.Now())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(M{"sample_rate": uplinkRate, "channels": 1, "format": "s16le", "events": outboundRecording.events, "wav_inserts_gaps": false, "server_receipt_confirmed": false, "capture_reason": outboundRecording.reason})
	})
}

const uplinkRecordingPage = `<!doctype html><html lang="ru"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Что слышит Gemini</title><style>body{font:18px system-ui;background:#eef6f7;color:#193b43;margin:0}main{max-width:680px;margin:40px auto;padding:20px}section{background:white;padding:24px;border-radius:20px}button,a{font:inherit}button{padding:13px 18px;margin:6px;border:0;border-radius:10px;background:#087e74;color:white;cursor:pointer}button:disabled{opacity:.45;cursor:default}audio{width:100%;margin-top:20px}p{line-height:1.5}small{display:block;line-height:1.5;color:#536d76}.links{margin:20px 0}.links a{margin-right:20px;color:#087e74}#state{min-height:3em}@media(max-width:420px){main{margin:12px auto;padding:14px}section{padding:18px}button{width:100%;margin:6px 0}}</style><main><h1>Что слышит Gemini</h1><p>Послушай точный звук, отправляемый колонкой в Gemini после всей обработки.</p><section><button id="start">Записать 30 секунд</button><button id="stop" disabled>Остановить</button><button id="clear">Удалить запись</button><p id="state" role="status">Загружаю…</p><audio id="player" controls hidden></audio><div class="links" id="links" hidden><a href="/diagnostics/uplink/audio.wav" download="gemini-uplink.wav">Скачать WAV</a><a href="/diagnostics/uplink/timeline.json" download="gemini-uplink-timeline.json">Границы и время пакетов</a></div><small>Нажми «Записать», затем скажи «Hey Google» и свою команду. Запись не включает микрофон сама: сохраняются только реально отправленные пакеты. Она хранится в памяти колонки до удаления, следующей записи или перезапуска.<br><br>Без усиления и нормализации. WAV содержит сэмплы подряд; паузы между отправками и границы фраз указаны отдельно. Успешная отправка клиентом не доказывает получение сервером.</small></section></main><script>const start=document.getElementById('start'),stop=document.getElementById('stop'),clear=document.getElementById('clear'),state=document.getElementById('state'),player=document.getElementById('player'),links=document.getElementById('links');let wasActive=false,loaded=false;async function update(){try{let r=await fetch('/diagnostics/uplink/status'),s=await r.json();start.disabled=s.active;stop.disabled=!s.active;state.textContent=s.active?'Запись: ещё '+s.remaining_seconds+' сек. Отправлено звука: '+s.audio_seconds.toFixed(1)+' сек.':s.bytes?'Готово: '+s.audio_seconds.toFixed(1)+' сек. отправленного звука.':'Записи пока нет.';let ready=!s.active&&s.bytes>0;player.hidden=links.hidden=!ready;if(ready&&(!loaded||wasActive)){player.src='/diagnostics/uplink/audio.wav?t='+Date.now();loaded=true}if(!s.bytes){loaded=false;player.removeAttribute('src')}wasActive=s.active}catch(e){state.textContent='Нет связи с колонкой.'}}async function action(name){try{let r=await fetch('/diagnostics/uplink/'+name,{method:'POST'});if(!r.ok)throw Error();if(name==='start'){loaded=false;player.pause()}await update()}catch(e){state.textContent='Не удалось выполнить действие.'}}start.onclick=()=>action('start');stop.onclick=()=>action('stop');clear.onclick=()=>action('clear');update();setInterval(update,700);</script></html>`
