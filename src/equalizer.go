package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// These are native Harman tone controls, in tenths of a dB. Do not expose
// their full driver range (-50..+50 dB), or DSP bypass/factory controls.
const eqBassControl = "HM BASS GAIN"
const eqTrebleControl = "HM TREBLE GAIN"

type equalizerState struct {
	Bass   float64 `json:"bass"`
	Treble float64 `json:"treble"`
}

type equalizerService struct {
	mu          sync.Mutex
	run         func(context.Context, ...string) ([]byte, error) // legacy isolated fixtures only
	castBaseURL string
	client      *http.Client
	path        string
	save        func([]byte) error // optional test seam; production uses writeAtomicFile
}

// Cast owns and persists its native user_eq. Never migrate the former ALSA
// equalizer.json: those cached values never represented the actual audio EQ.
var speakerEqualizer = &equalizerService{
	castBaseURL: citationCastEqualizerURL(),
	client:      &http.Client{Timeout: 3 * time.Second},
	path:        filepath.Join(stateDirectory, "cast-equalizer.json"),
}
var defaultSpeakerEqualizer = speakerEqualizer

func citationCastEqualizerURL() string {
	// The stock receiver starts with zero user EQ bands. The bind mount
	// disappears at reboot, so fail closed until the EQ launcher is active.
	overlay, err := os.Stat("/data/opt/citation-gemini/bin/cast-receiver-eq.sh")
	if err != nil {
		return ""
	}
	launcher, err := os.Stat("/sbin/cast_receiver.sh")
	if err != nil || !os.SameFile(overlay, launcher) {
		return ""
	}
	return "http://127.0.0.1:8008"
}

func (e *equalizerService) castRequest(ctx context.Context, method, path string, body, out any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(e.castBaseURL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := e.client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("native Cast EQ unavailable: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Do not echo an unfiltered setup response containing private device info.
		return fmt.Errorf("native Cast EQ HTTP %d", response.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(out)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 65536))
	return err
}

func (e *equalizerService) readCastLocked(ctx context.Context) (equalizerState, error) {
	var response struct {
		UserEQ *struct {
			LowShelf *struct {
				Gain *float64 `json:"gain_db"`
			} `json:"low_shelf"`
			HighShelf *struct {
				Gain *float64 `json:"gain_db"`
			} `json:"high_shelf"`
		} `json:"user_eq"`
		Supported *bool `json:"user_eq_supported"`
	}
	if err := e.castRequest(ctx, http.MethodGet, "/setup/eureka_info?params=user_eq,user_eq_supported", nil, &response); err != nil {
		return equalizerState{}, err
	}
	if response.Supported != nil && !*response.Supported {
		return equalizerState{}, errors.New("native Cast user EQ is not supported")
	}
	q := response.UserEQ
	if q == nil || q.LowShelf == nil || q.HighShelf == nil || q.LowShelf.Gain == nil || q.HighShelf.Gain == nil {
		return equalizerState{}, errors.New("native Cast user EQ shelves are missing")
	}
	result := equalizerState{Bass: *q.LowShelf.Gain, Treble: *q.HighShelf.Gain}
	if math.IsNaN(result.Bass) || math.IsInf(result.Bass, 0) || math.IsNaN(result.Treble) || math.IsInf(result.Treble, 0) {
		return equalizerState{}, errors.New("invalid native Cast user EQ values")
	}
	return result, nil
}

func runEqualizerAmixer(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// Stop option parsing before cset's negative numeric value (e.g. -5).
	return exec.CommandContext(ctx, "amixer", append([]string{"--"}, args...)...).Output()
}

var eqValuePattern = regexp.MustCompile(`(?m)^\s*:\s*values=(-?[0-9]+)\s*$`)
var eqInfoPattern = regexp.MustCompile(`type=INTEGER,access=[^,]*rw[^,]*,values=1,min=(-?[0-9]+),max=(-?[0-9]+),step=1`)

func parseEqualizerControl(data []byte) (int, error) {
	info := eqInfoPattern.FindSubmatch(data)
	value := eqValuePattern.FindSubmatch(data)
	if len(info) != 3 || len(value) != 2 {
		return 0, errors.New("unexpected Harman tone control format")
	}
	minimum, e1 := strconv.Atoi(string(info[1]))
	maximum, e2 := strconv.Atoi(string(info[2]))
	raw, e3 := strconv.Atoi(string(value[1]))
	if e1 != nil || e2 != nil || e3 != nil || minimum != -500 || maximum != 500 || raw < minimum || raw > maximum || !strings.Contains(string(data), "dBminmax-min=-50.00dB,max=50.00dB") {
		return 0, errors.New("unexpected Harman tone control range")
	}
	return raw, nil
}

func equalizerUnits(value float64) (int, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < -12 || value > 12 || math.Abs(value*2-math.Round(value*2)) > 1e-8 {
		return 0, errors.New("выбери значение от −12 до +12 дБ с шагом 0,5 дБ")
	}
	return int(math.Round(value * 10)), nil
}

func (e *equalizerService) readLocked(ctx context.Context) (equalizerState, error) {
	if e.castBaseURL != "" {
		return e.readCastLocked(ctx)
	}
	var result equalizerState
	for _, control := range []string{eqBassControl, eqTrebleControl} {
		data, err := e.run(ctx, "cget", "name="+control)
		if err != nil {
			return result, fmt.Errorf("read %s: %w", control, err)
		}
		value, err := parseEqualizerControl(data)
		if err != nil {
			return result, err
		}
		if control == eqBassControl {
			result.Bass = float64(value) / 10
		} else {
			result.Treble = float64(value) / 10
		}
	}
	return result, nil
}

func (e *equalizerService) state(ctx context.Context) (equalizerState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.readLocked(ctx)
}

func (e *equalizerService) writeLocked(ctx context.Context, state equalizerState) error {
	if e.castBaseURL != "" {
		// The native endpoint updates the supplied shelf fields. Gain-only
		// updates retain frequency, quality and any existing peaking bands.
		return e.castRequest(ctx, http.MethodPost, "/setup/user_eq/set_equalizer", map[string]any{
			"low_shelf":  map[string]any{"gain_db": state.Bass},
			"high_shelf": map[string]any{"gain_db": state.Treble},
		}, nil)
	}
	for i, control := range []string{eqBassControl, eqTrebleControl} {
		value := state.Bass
		if i == 1 {
			value = state.Treble
		}
		// Control names are fixed, and values are integers, never shell text.
		if _, err := e.run(ctx, "cset", "name="+control, strconv.Itoa(int(math.Round(value*10)))); err != nil {
			return fmt.Errorf("write %s: %w", control, err)
		}
	}
	return nil
}

func (e *equalizerService) applyLocked(ctx context.Context, want equalizerState, persist bool) (equalizerState, error) {
	if _, err := equalizerUnits(want.Bass); err != nil {
		return equalizerState{}, err
	}
	if _, err := equalizerUnits(want.Treble); err != nil {
		return equalizerState{}, err
	}
	before, err := e.readLocked(ctx)
	if err != nil {
		return before, err
	}
	err = e.writeLocked(ctx, want)
	if err == nil {
		var actual equalizerState
		actual, err = e.readLocked(ctx)
		if err == nil && actual != want {
			err = errors.New("Harman tone readback did not match requested values")
		}
	}
	if err == nil && persist {
		data, _ := json.Marshal(want)
		if e.save != nil {
			err = e.save(data)
		} else {
			err = writeAtomicFile(e.path, data, 0600)
		}
	}
	if err == nil {
		return want, nil
	}
	// A disconnected HTTP client must not prevent restoration of both controls.
	rollbackCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	rollbackErr := e.writeLocked(rollbackCtx, before)
	if rollbackErr == nil {
		var actual equalizerState
		actual, rollbackErr = e.readLocked(rollbackCtx)
		if rollbackErr == nil && actual != before {
			rollbackErr = errors.New("rollback readback mismatch")
		}
	}
	if rollbackErr != nil {
		return before, fmt.Errorf("apply failed: %v; rollback failed: %w", err, rollbackErr)
	}
	return before, fmt.Errorf("apply failed; previous tone restored: %w", err)
}

func (e *equalizerService) apply(ctx context.Context, want equalizerState) (equalizerState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.applyLocked(ctx, want, true)
}

// Call once during startup, after the native audio service is available. The
// lock prevents a delayed startup retry from overwriting a later user's change.
func restoreEqualizer() {
	e := speakerEqualizer
	if e.castBaseURL != "" {
		// Cast's own persisted user_eq is authoritative. Replaying our last
		// UI record would overwrite later Google Home/native changes.
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	data, err := os.ReadFile(e.path)
	if os.IsNotExist(err) {
		return
	}
	var saved equalizerState
	if err != nil || json.Unmarshal(data, &saved) != nil {
		log.Println("equalizer: saved settings could not be read")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		if _, err = e.applyLocked(ctx, saved, false); err == nil {
			return
		}
		if ctx.Err() != nil || attempt == 2 {
			break
		}
		if waitSpotifyRegistration(ctx, time.Second) != nil {
			break
		}
	}
	log.Println("equalizer restore:", err)
}

func registerEqualizerHandlers() {
	http.HandleFunc("/equalizer", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/equalizer" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, equalizerPage)
	})
	http.HandleFunc("/equalizer/state", equalizerStateHandler)
}

func equalizerStateHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The running Harman library rejects the bass/treble parameter IDs. ALSA
	// readback is only a cached control value, not an applied audible EQ.
	if speakerEqualizer == defaultSpeakerEqualizer && speakerEqualizer.castBaseURL == "" {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "available": false, "error": "Эквалайзер недоступен: прошивка колонки сообщает, что регулировка баса и высоких отключена."})
		return
	}
	var state equalizerState
	var err error
	switch r.Method {
	case http.MethodGet:
		state, err = speakerEqualizer.state(r.Context())
	case http.MethodPost:
		if !localSpotifyForm(r) {
			http.Error(w, `{"ok":false,"error":"Origin rejected"}`, http.StatusForbidden)
			return
		}
		var request struct {
			Bass   *float64 `json:"bass"`
			Treble *float64 `json:"treble"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || request.Bass == nil || request.Treble == nil {
			http.Error(w, `{"ok":false,"error":"Нужны значения баса и высоких"}`, http.StatusBadRequest)
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			http.Error(w, `{"ok":false,"error":"Некорректные настройки"}`, http.StatusBadRequest)
			return
		}
		state = equalizerState{Bass: *request.Bass, Treble: *request.Treble}
		if _, err = equalizerUnits(state.Bass); err == nil {
			_, err = equalizerUnits(state.Treble)
		}
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(M{"ok": false, "error": err.Error()})
			return
		}
		state, err = speakerEqualizer.apply(r.Context(), state)
	default:
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		log.Println("equalizer:", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		message := "Не удалось прочитать настройки звука. Попробуй ещё раз."
		if r.Method == http.MethodPost {
			message = "Изменения не применены. Предыдущие настройки восстановлены."
			if strings.Contains(err.Error(), "rollback failed") {
				message = "Не удалось применить и восстановить настройки. Обнови страницу и проверь звук."
			}
		}
		json.NewEncoder(w).Encode(M{"ok": false, "error": message})
		return
	}
	json.NewEncoder(w).Encode(M{"ok": true, "bass": state.Bass, "treble": state.Treble})
}

const equalizerPage = `<!doctype html><html lang="ru"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Звук · Citation</title>
<style>*{box-sizing:border-box}body{margin:0;background:#edf6f7;color:#153344;font:17px system-ui,sans-serif}main{max-width:680px;margin:40px auto;padding:20px}.tag{color:#087b7c;font-size:13px;letter-spacing:.15em;font-weight:750}h1{font-size:38px;line-height:1.1;margin:12px 0}p{color:#55717c;line-height:1.5}.card{background:white;border:1px solid #d7e9eb;border-radius:24px;padding:26px;margin-top:24px;box-shadow:0 12px 36px #1e59620d}.row{display:flex;justify-content:space-between;gap:12px;align-items:center;margin-top:16px}label{font-size:21px;font-weight:650}output{font-variant-numeric:tabular-nums;background:#e3f4f2;color:#086f67;padding:7px 12px;border-radius:12px;white-space:nowrap}input[type=range]{display:block;width:100%;min-height:44px;margin:14px 0 0;accent-color:#07877c}.ends{display:flex;justify-content:space-between;font-size:13px;color:#6d838a;margin-bottom:26px}.presets{display:flex;gap:10px;flex-wrap:wrap}button{font:inherit;min-height:46px;border-radius:12px;padding:10px 15px;cursor:pointer;border:1px solid #c7dddf;background:#f3f9f9;color:#1b4e59}button.primary{background:#087e74;color:white;border-color:#087e74;width:100%;margin-top:24px;font-weight:700}button:disabled{cursor:wait;opacity:.5}button:focus-visible,input:focus-visible{outline:3px solid #eea842;outline-offset:3px}#status{min-height:48px;font-size:15px;line-height:1.5;margin:18px 0 0;color:#357062}#status.error{color:#a32936}.note{font-size:14px;margin-bottom:0}@media(max-width:420px){main{margin:14px auto;padding:14px}.card{padding:20px}h1{font-size:32px}.presets button{flex:1 1 42%}}</style>
<main><div class="tag">CITATION · ТВОЙ ЗВУК</div><h1>Как тебе нравится</h1><p>Добавь немного баса или сделай звучание мягче.<br>Громкость останется прежней.</p><section class="card" aria-label="Эквалайзер">
<div class="row"><label for="bass">Бас</label><output id="bassValue" for="bass">0 дБ</output></div><input disabled id="bass" type="range" min="-12" max="12" step="0.5" value="0"><div class="ends"><span>Легче</span><span>Глубже</span></div>
<div class="row"><label for="treble">Высокие</label><output id="trebleValue" for="treble">0 дБ</output></div><input disabled id="treble" type="range" min="-12" max="12" step="0.5" value="0"><div class="ends"><span>Мягче</span><span>Ярче</span></div>
<div class="presets" aria-label="Готовые настройки"><button disabled data-bass="6" data-treble="0">Больше баса</button><button disabled data-bass="0" data-treble="6">Больше высоких</button><button disabled data-bass="2" data-treble="-6">Теплее</button><button disabled data-bass="-10" data-treble="2">Чётче</button><button disabled data-bass="0" data-treble="-10">Мягче</button><button disabled data-bass="0" data-treble="0">Заводское звучание</button></div>
<button disabled class="primary" id="apply">Применить</button><div id="status" role="status" aria-live="polite">Загружаю настройки…</div><p class="note">Настройки сохраняются на колонке.</p></section></main>
<script>const bass=document.getElementById('bass'),treble=document.getElementById('treble'),statusBox=document.getElementById('status');let ready=false,busy=false;function format(v){return(v>0?'+':'')+v.toLocaleString('ru-RU')+' дБ'}function show(){document.getElementById('bassValue').textContent=format(+bass.value);document.getElementById('trebleValue').textContent=format(+treble.value)}function lock(v){busy=v;document.querySelectorAll('button,input').forEach(e=>e.disabled=v||!ready)}function message(t,error=false){statusBox.textContent=t;statusBox.classList.toggle('error',error)}async function api(options){let response=await fetch('/equalizer/state',options),data=await response.json();if(!response.ok||!data.ok)throw Error(data.error||'Не удалось связаться с колонкой');return data}async function load(){let s=await api();bass.value=s.bass;treble.value=s.treble;show();ready=true;lock(false)}async function apply(){if(busy||!ready)return;lock(true);message('Применяю…');try{let s=await api({method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({bass:+bass.value,treble:+treble.value})});bass.value=s.bass;treble.value=s.treble;show();message('Готово. Настройки сохранены.')}catch(e){try{await load()}catch{}message(e.message,true)}finally{lock(false)}}[bass,treble].forEach(e=>e.addEventListener('input',()=>{show();message('Нажми «Применить», чтобы услышать изменения.')}));document.getElementById('apply').onclick=apply;document.querySelectorAll('[data-bass]').forEach(e=>e.onclick=()=>{bass.value=e.dataset.bass;treble.value=e.dataset.treble;show();apply()});load().then(()=>message('Можно настроить звук.')).catch(e=>message(e.message+' Обнови страницу, чтобы повторить.',true));</script></html>`
