package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/http2"
)

type M = map[string]any
type Config struct {
	KeyFile           string  `json:"key_file"`
	Model             string  `json:"model"`
	AlsaConfig        string  `json:"alsa_config"`
	Playback          string  `json:"playback"`
	AutoWake          bool    `json:"auto_wake"`
	Voice             string  `json:"voice"`
	MicrophoneGain    float64 `json:"microphone_gain"`
	MicrophoneBackend string  `json:"microphone_backend"`
	TestTrace         bool    `json:"test_trace"`
	NativeBehavior    bool    `json:"native_behavior"`
	ReplaceGoogle     bool    `json:"replace_google"`
	DJWake            bool    `json:"dj_wake"`

	// StockDynamicGain enables the recovered libassistant AutoGainControl
	// policy. A nil pointer means "use the stock default", which is enabled.
	StockDynamicGain    *bool    `json:"stock_dynamic_gain"`
	StockDynamicGainMax *float64 `json:"stock_dynamic_gain_max"`
}

var cfg Config
var key string

func clean(err error) string {
	if err == nil {
		return ""
	}
	if key == "" {
		return err.Error()
	}
	return strings.ReplaceAll(err.Error(), key, "[REDACTED]")
}
func cloud() (*websocket.Conn, error) {
	return cloudContext(context.Background())
}
func cloudContext(ctx context.Context) (*websocket.Conn, error) {
	return cloudContextActivity(ctx, false)
}
func cloudContextActivity(ctx context.Context, manualActivity bool, musicPlaying ...bool) (*websocket.Conn, error) {
	netDialer := &net.Dialer{
		Timeout:   12 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	d := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return netDialer.DialContext(ctx, "tcp4", addr)
		},
	}
	endpoint := "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"
	c, _, err := d.DialContext(ctx, endpoint, http.Header{"X-Goog-Api-Key": []string{key}})
	if err != nil {
		log.Println("Gemini Live dial retry:", clean(err))
		c, _, err = d.DialContext(ctx, endpoint, http.Header{"X-Goog-Api-Key": []string{key}})
		if err != nil {
			return nil, err
		}
	}
	setupDone := make(chan struct{})
	defer close(setupDone)
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-setupDone:
		}
	}()
	c.SetReadLimit(4 << 20)
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	setup := M{"setup": M{"model": cfg.Model, "generationConfig": M{"responseModalities": []string{"AUDIO"}}, "systemInstruction": M{"parts": []M{{"text": "Ты умная домашняя колонка Harman Kardon с голосовым ассистентом. " +
		"Говори о себе естественно. " +
		"Отвечай на языке пользователя; поддерживай русский и английский. На русскую просьбу отвечай по-русски, на английскую — по-английски; при смешанной речи выбери основной язык фразы. English commands pause/resume/next/previous/volume/weather work natively. Короткие слова и сленг не считай иностранными; если транскрипт двусмысленный, переспроси, не завершай сессию. " +
		"Отвечай кратко, емко, естественно, дружелюбно и без лишних вводных слов. Сразу переходи к сути ответа. " +
		"На простой вопрос обычно достаточно одного-двух коротких предложений. Не добавляй дежурные предложения помощи и повтор вопроса. Подробности давай по просьбе или когда они необходимы; обязательные цитаты и важные условия сохраняй. " +
		"Перед долгим поиском через web_search или get_news один раз коротко озвучь, что делаешь: например «Ищу в интернете» или «Проверяю новости», затем сразу вызывай инструмент в этом же ходе. Это промежуточная реплика, не окончательный ответ: после результата сообщи найденное. Не повторяй предупреждение при нескольких инструментах для одного запроса, не обещай конкретное время и не говори, что результат уже найден. Сохраняй язык пользователя. Для времени, громкости, паузы и других быстрых команд промежуточные фразы не нужны. На обычные вопросы из общих знаний отвечай сразу, без ненужного поиска. " +
		"На одно только приветствие «Hey Google» без вопроса — просто спокойно жди вопроса пользователя и не представляйся. " +
		"На просьбу сказать или повторить слово/фразу произнеси её сам, без вызова инструментов. Название сервиса само по себе не является командой запуска: если действие неясно, уточни. " +
		"Громкостью и воспроизведением управляй только через функции. Команда стоп означает остановить ответ. Для голосовых команд громкости используй шкалу 0–10 или проценты. После успешного изменения колонка подаёт штатный сигнал."}}}, "tools": []M{{"functionDeclarations": []M{
		{"name": "set_volume", "description": "Set speaker volume. Pass percent 0..100 for spoken commands: «громкость 5» is level 5 of 10, so percent=50; «громкость 50» or «50 процентов» also means percent=50. Explicit percentages take precedence. Use level only for an explicitly requested native step 0..48.", "parameters": M{"type": "OBJECT", "properties": M{"percent": M{"type": "INTEGER"}, "level": M{"type": "INTEGER"}}}},
		{"name": "get_volume", "description": "Read the current speaker volume. Use for «какая сейчас громкость?» or «на какой громкости?». Report the returned percent.", "parameters": M{"type": "OBJECT", "properties": M{}}},
		{"name": "adjust_volume", "description": "Adjust speaker volume in native steps (-6 to +6). For «потише» or «тише» use -3. For «погромче» or «громче» use +3.", "parameters": M{"type": "OBJECT", "properties": M{"delta": M{"type": "INTEGER", "minimum": -6, "maximum": 6}}, "required": []string{"delta"}}},
		{"name": "media_control", "description": "Control existing speaker music playback", "parameters": M{"type": "OBJECT", "properties": M{"action": M{"type": "STRING", "enum": []string{"pause", "resume", "next", "previous", "stop"}}}, "required": []string{"action"}}},
		{"name": "spotify_dj_prompt", "description": "Send a live voice request to Spotify AI DJ (use whenever the user addresses DJ or asks DJ to play/change something: «диджей включи ...», «диджей поставь ...», «эй диджей ...», «попроси диджея ...», «спроси у диджея ...», «скажи диджею ...», «в диджее включи ...», «DJ play ...», «hey DJ ...»). IMPORTANT: Spotify AI DJ only understands English prompts! You MUST translate the user's Russian request into a concise, natural English DJ prompt in the `prompt` parameter (for example: «диджей включи что-нибудь новое» -> prompt='play something new and fresh', «диджей поставь весёлый рок» -> prompt='upbeat energetic rock', «диджей что-нибудь спокойное на вечер» -> prompt='chill relaxing evening vibes', «диджей включи хиты девяностых» -> prompt='90s hits'). Do NOT call play_music when the user addresses DJ with a prompt/vibe — ALWAYS call spotify_dj_prompt with an English prompt!", "parameters": M{"type": "OBJECT", "properties": M{"prompt": M{"type": "STRING", "description": "The music prompt or vibe for Spotify DJ, ALWAYS translated into concise natural English (e.g. 'play something new', 'upbeat electronic music', 'chill late night vibes')."}}, "required": []string{"prompt"}}},
	}}}}}
	voice := cfg.Voice
	if voice == "" {
		voice = "Charon"
	}
	setup["setup"].(M)["generationConfig"].(M)["speechConfig"] = M{"voiceConfig": M{"prebuiltVoiceConfig": M{"voiceName": voice}}}
	setup["setup"].(M)["realtimeInputConfig"] = M{"automaticActivityDetection": M{"startOfSpeechSensitivity": "START_SENSITIVITY_HIGH", "endOfSpeechSensitivity": "END_SENSITIVITY_LOW", "prefixPaddingMs": 300, "silenceDurationMs": 700}, "turnCoverage": "TURN_INCLUDES_ONLY_ACTIVITY"}
	if manualActivity {
		setup["setup"].(M)["realtimeInputConfig"] = M{"automaticActivityDetection": M{"disabled": true}, "turnCoverage": "TURN_INCLUDES_ALL_INPUT"}
	}
	// Bias the live input transcription toward the user's languages and short
	// Russian words that auto-detection can misread. language_codes and
	// custom_vocabulary are supported by the Live API AudioTranscriptionConfig.
	setup["setup"].(M)["inputAudioTranscription"] = M{
		"languageCodes": []string{"ru-RU", "en-US"},
		"customVocabulary": []string{
			"включи", "выключи", "поставь", "пауза", "продолжи", "дальше", "назад",
			"громче", "тише", "громкость", "музыка", "плейлист", "альбом", "исполнитель",
			"погода", "время", "таймер", "будильник", "напомни", "новости", "календарь",
			"спасибо", "пожалуйста", "ладно", "да", "нет", "хорошо", "окей",
		},
	}
	setup["setup"].(M)["outputAudioTranscription"] = M{}
	declarations := setup["setup"].(M)["tools"].([]M)[0]["functionDeclarations"].([]M)
	declarations = append(declarations, M{
		"name":        "play_music",
		"description": "Play music from Spotify (tracks, playlists, albums, artists). For an explicitly named playlist use music_type='playlist', playlist_mode='named' and the exact title. For a generic request («включи музыку», «play something») use music_type='playlist', playlist_mode='mood' and an empty track; the speaker chooses a playlist from the user's library. For mood, genre, activity or vibe requests use music_type='playlist', playlist_mode='mood' and the mood in track. For DJ use music_type='playlist', playlist_mode='named', track='DJ'. For daylist use track='daylist'. Do not invent a playlist title or URI. If a title/artist is mentioned, put the exact title in track and the artist in artist. Never ask what to play for a generic request.",
		"parameters": M{
			"type": "OBJECT",
			"properties": M{
				"track":         M{"type": "STRING", "description": "Song/album/playlist title, or mood/genre keywords. For 'Удиви меня' or 'Surprise me', call this tool with track='удиви меня', music_type='playlist', playlist_mode='mood' to play random music. Use empty string for a plain 'play music'."},
				"artist":        M{"type": "STRING", "description": "Artist name if mentioned"},
				"music_type":    M{"type": "STRING", "enum": []string{"track", "playlist", "album", "artist"}},
				"playlist_mode": M{"type": "STRING", "enum": []string{"named", "mood"}, "description": "Use mood for atmosphere, genre, activity or recommendations; named only for an explicitly named playlist."},
				"search_query":  M{"type": "STRING", "description": "For mood playlists, optional short semantic search keywords in the user's language or English, such as epic orchestral. Do not invent a playlist title or ID. Omit for named playlists."},
			},
		},
	})
	setup["setup"].(M)["tools"].([]M)[0]["functionDeclarations"] = declarations
	isMusic := len(musicPlaying) > 0 && musicPlaying[0]
	declarations = append(declarations,
		M{"name": "end_session", "description": "Завершить разговор и закрыть микрофон сразу после текущего ответа. Вызывай end_session, когда разговор явно закончен: прощание, «спасибо», «хватит», «отбой», «пока», «всё», «стоп», «не хочу», «не надо», либо пользователь явно отказывается продолжать. Также вызывай при командах управления музыкой и громкостью и когда играет музыка и нужно вернуть её громкость после короткого ответа. На обычный вопрос или ответ end_session не обязателен: окно продолжения и таймер простоя закроют сессию сами, и микрофон не нужно держать открытым ради случайного фонового шума. КАТЕГОРИЧЕСКИ ЗАПРЕЩЕНО вызывать end_session, если ты не расслышал речь или переспрашиваешь.", "parameters": M{"type": "OBJECT", "properties": M{}}},
		M{"name": "get_time", "description": "Get the real current local time and date. Returns time/date/weekday plus `iso`/`local_iso` RFC3339 with the correct offset. Always use for current time/date and for converting reminder/calendar times, including colloquial Russian «Сколько время?», «Сколько времени?», «Который час?» and «Какое сейчас время?». A question about the duration of an event is different.", "parameters": M{"type": "OBJECT", "properties": M{"timezone": M{"type": "STRING", "description": "Optional IANA timezone; omit for speaker local time."}}}},
		M{"name": "get_current_music", "description": "Read the current Spotify track, artist and playlist/context on this speaker. Use for «Что сейчас играет?», «Что за песня?», «Кто поёт?», «Что за плейлист играет?» and 'What song or playlist is playing?'. This only reads playback. If Spotify is paused, say so. If the result has playlist or context, mention it when asked.", "parameters": M{"type": "OBJECT", "properties": M{}}},
		M{"name": "spotify_playback", "description": "Spotify on this speaker. «Добавь в любимые», «лайкни песню» -> save_current. «Что дальше?», «Что в очереди?» -> next_in_queue (read only, do not skip). «Шафл» -> shuffle_toggle; «включи/выключи шафл» -> shuffle_on/shuffle_off. «Повтори трек» -> repeat_track; «повторяй плейлист» -> repeat_context; «выключи повтор» -> repeat_off. Ask status of shuffle or repeat -> playback_modes. Report the actual result briefly.", "parameters": M{"type": "OBJECT", "properties": M{"action": M{"type": "STRING", "enum": []string{"save_current", "next_in_queue", "shuffle_on", "shuffle_off", "shuffle_toggle", "repeat_track", "repeat_context", "repeat_off", "playback_modes"}}}, "required": []string{"action"}}},
		M{"name": "get_weather", "description": "Get live weather. If city not specified by user, use the saved home city; if none is saved, ask the user for a city.", "parameters": M{"type": "OBJECT", "properties": M{"city": M{"type": "STRING"}}}},
		M{"name": "set_home_city", "description": "Remember the city the user explicitly chooses as the default for weather. Do not change home city for a one-off forecast in another city.", "parameters": M{"type": "OBJECT", "properties": M{"city": M{"type": "STRING"}}, "required": []string{"city"}}})
	declarations = append(declarations, searchToolDeclarations()...)
	declarations = append(declarations, timerToolDeclarations()...)
	declarations = append(declarations, ecosystemToolDeclarations()...)
	declarations = append(declarations, memoryToolDeclarations()...)
	declarations = append(declarations, newsToolDeclarations()...)
	declarations = append(declarations, broadcastToolDeclarations()...)
	declarations = append(declarations, routineToolDeclarations()...)
	declarations = append(declarations, noteToolDeclarations()...)
	setup["setup"].(M)["tools"].([]M)[0]["functionDeclarations"] = declarations
	parts := setup["setup"].(M)["systemInstruction"].(M)["parts"].([]M)
	parts[0]["text"] = parts[0]["text"].(string) + " Если язык фразы неясен, попробуй понять её на слух как русскую или английскую. Слушай саму речь. Например, «Сколько время?», «Сколько времени?» и «Который час?» — обычные вопросы о текущем времени. Если слышно вопрос о длительности, уточни о каком событии речь. Для текущего времени и погоды всегда вызывай get_time и get_weather, не выдумывай значения. " +
		"Для текущих событий, цен, спорта, новостей и любых данных, которые могли измениться, вызывай web_search. Запрос в web_search формулируй на языке пользователя, коротко и конкретно, сохраняя ключевые слова вопроса; не выбрасывай слова и не подменяй тему. Если результат поиска не относится к вопросу, не цитируй его, а ответь из общих знаний с оговоркой, что данные могут быть неактуальны. В ответе используй только готовое поле answer: произнеси его один раз, близко к тексту, и закончи. Не повторяй цитату, не пересказывай её после и не называй источник второй раз: в answer уже есть источник. Не переставляй и не склеивай обрывки. Ссылки не читай. Для курсов и конвертации валют всегда вызывай convert_currency. Для конвертаций единиц вызывай convert_units; для курсов и конвертации валют — convert_currency, не web_search. Не говори, что у тебя нет доступа к интернету: используй инструмент. " +
		"Команды find_phone, create_google_reminder, create_calendar_event, set_reminder, list_reminders и cancel_reminder относятся к личным данным и доступны напрямую. Не проси Voice Match, пароль, имя или английскую фразу; выполняй запрос сразу. Ассистент работает в фоне без звука; всегда отвечай ты, Gemini. Если find_phone вернул needs_repeat, попроси пользователя повторить по-английски «find my phone» и не говори, что телефон уже звонит. " +
		"Для напоминаний и календаря сначала вызови get_time, чтобы знать текущую дату и время. get_time возвращает `iso`/`local_iso` с правильным смещением часового пояса; передавай это значение в at без добавления Z. В list_reminders поле `at` уже локальное RFC3339 со смещением, а `at_utc` — UTC; у каждого элемента также есть `local_time`, `local_date`, `weekday`, `timezone` и `when`; называй именно локальные поля. Текст напоминания и название события оставляй на языке пользователя, не переводи их без просьбы. Если инструмент вернул source=local или offline, скажи, что создано локально и синхронизируется с Google после авторизации; если source=task или calendar — что запись уже в Google. Для списка напоминаний и ближайших событий используй list_reminders. " +
		"Для новостей вызывай get_news; не выдумывай заголовки. Для «включи новости» кратко назови 5 главных заголовков на языке пользователя и источники. Для «мои новости», «по моим темам» вызывай get_news с personal=true. Если пользователь просит запомнить новостные темы, вызывай remember с category='news_topic'. Только для явной просьбы передать объявление на другие домашние колонки используй broadcast. Подтверждай отправку лишь при ok=true; обычное «скажи» или «повтори» выполняй своим голосом без broadcast. Для «доброе утро», «спокойной ночи», «я ухожу», «я дома» вызывай run_routine и затем произнеси текст из instruction. Для заметок и списков используй create_note/add_list_item/read_note/list_notes/complete_list_item/delete_note; это личные данные, доступны напрямую. При авторизованном Google OAuth они синхронизируются с Google Tasks, иначе остаются локальными. Если просят позвонить, скажи, что эта колонка не поддерживает звонки, и предложи позвонить с телефона. Не выдумывай управление умным домом, если инструмент не вернул успех. " +
		"Перед использованием личных предпочтений, памяти, календаря, напоминаний или любых личных данных вызывай get_personal_context. Используй memories естественно и не упоминай хранилище. Не проси Voice Match, пароль, имя или английскую фразу. На общие вопросы отвечай нормально. " +
		"Личные инструменты можно вызывать сразу, без дополнительной проверки. " +
		"Запоминай факты о пользователе только по явной просьбе («запомни, что...») через remember. Никогда не сохраняй пароли, номера карт, PIN, медицинские данные и другие секреты. " +
		"В погоде называй температуру строго целыми числами без десятых долей (например 'плюс пять градусов', но ни в коем случае не 'пять целых две десятых'). " +
		"Если пользователь спрашивает погоду и город не назван — используй сохранённый домашний город из preferences. Если город не сохранён, коротко уточни его; не выдумывай. Вызывай get_weather только когда пользователь действительно спрашивает про погоду или температуру, и никогда не вызывай функции на фразы отказа («не надо», «мне не надо», «хватит», «отбой») или неразборчивый шум. " +
		"Если пользователь обращается к диджею с пожеланием («диджей, включи что-нибудь новое», «эй диджей, поставь рок», «попроси диджея включить что-то спокойное», «скажи диджею ...») — ВСЕГДА вызывай функцию spotify_dj_prompt и ОБЯЗАТЕЛЬНО переводи запрос в параметре prompt на естественный английский язык (например 'play something new', 'upbeat rock', 'something chill and relaxing'), так как Spotify DJ понимает только английский! Если же пользователь просто говорит «включи диджея» без пожеланий — вызывай play_music(music_type='playlist', playlist_mode='named', track='DJ'). " +
		"Если пользователь просит музыку по настроению, жанру или атмосфере (например 'что-нибудь фэмили френдли', 'спокойное', 'для фона', 'рок', 'веселое') без упоминания диджея — ВСЕГДА вызывай play_music с music_type='playlist' и темой в track, никогда не переспрашивай! " +
		"Если музыка на колонке уже играла или стоит на паузе, и пользователь говорит «play», «играй», «продолжи», «возобнови», «дальше», «музыку» — ВСЕГДА вызывай media_control(action='resume'), ни в коем случае НЕ вызывай play_music, чтобы не сбросить текущую очередь треков! " +
		"Если пользователь просит включить музыку вообще ('включи музыку', 'вруби музыку', 'сыграй что-нибудь') без конкретного названия — НИКОГДА не переспрашивай какую музыку включить! Сразу вызывай функцию play_music с music_type='playlist' и пустым track. " +
		"На отдельную команду «Удиви меня» или «Surprise me» сразу вызови play_music с music_type=playlist, playlist_mode=mood, track='удиви меня'. Это просьба включить случайную персональную подборку Spotify. После запроса музыки сразу вызови play_music. Если функция вернула ready_to_play, произнеси только короткую фразу из announcement. Если функция вернула ошибку, кратко объясни её. Переноси название и исполнителя точно из просьбы, не меняй песню на похожую. Не повторяй функцию без новой команды."
	parts[0]["text"] = parts[0]["text"].(string) + " ПРАВИЛА ЗАВЕРШЕНИЯ РАЗГОВОРА (функция end_session): " +
		"1) При музыке: если на колонке играет музыка, она приглушена на время ответа. Вызывай end_session при явных словах завершения («спасибо», «хватит») или разовых командах плеера/громкости. На обычные вопросы, факты и диалог НЕ вызывай end_session — дай пользователю возможность задать уточняющий вопрос или ответить. " +
		"2) Без музыки: вызывай end_session, когда разговор явно завершён — прощание или слова завершения («спасибо», «понял», «всё», «пока», «отбой», «хватит»), либо явный отказ или безразличие («не хочу», «не надо», «стоп», «ладно, всё»). Если пользователь отказался от твоего предложения продолжить, не предлагай снова и не жди — вызови end_session и коротко попрощайся. На обычный ответ или вопрос end_session не нужен: окно продолжения и таймер простоя закроют сессию сами, а микрофон не должен оставаться открытым ради случайного фонового шума. " +
		"Для коротких команд управления музыкой и громкостью достаточно действия, без лишнего речевого подтверждения. Не предлагай помощь без необходимости. " + recentConversation.instruction(time.Now())
	parts[0]["text"] = parts[0]["text"].(string) + voicePersona(voice)
	if isMusic {
		text := "\nВАЖНО: Сейчас на колонке играет музыка (она временно приглушена)."
		if name, _, typ := directSpotify.playbackContext(); name != "" {
			switch typ {
			case "playlist":
				text += fmt.Sprintf(" Текущий плейлист: «%s».", name)
			case "dj":
				text += " Сейчас играет Spotify DJ."
			case "mood":
				text += fmt.Sprintf(" Сейчас играет подборка по настроению «%s».", name)
			case "album":
				text += fmt.Sprintf(" Сейчас играет альбом «%s».", name)
			case "artist":
				text += fmt.Sprintf(" Сейчас играет музыка исполнителя «%s».", name)
			case "track":
				text += fmt.Sprintf(" Сейчас играет трек «%s».", name)
			}
		}
		if native := readSpotifyNative(); native.Track.Name != "" {
			text += fmt.Sprintf(" Текущий трек: «%s» — %s.", native.Track.Name, strings.Join(native.Track.ArtistNames, ", "))
		}
		text += " Отвечай кратко и емко. Если пользователь задает вопрос или просит что-то рассказать — ответь и жди возможных уточнений, не вызывай end_session сам. Если спрашивают, какой плейлист или подборка играет, вызывай get_current_music и назови поле playlist или context."
		parts[0]["text"] = parts[0]["text"].(string) + text
	}
	if isRecentDJWake() {
		parts[0]["text"] = parts[0]["text"].(string) + "\nВАЖНО: Эта сессия активирована напрямую голосовым ключевым словом «Диджей» (Hey DJ). Пользователь сейчас обращается непосредственно к Spotify AI DJ! Любое музыкальное пожелание, жанр, настроение, темп или просьбу в этой реплике (например «включи что-нибудь новое», «поставь рок», «что-нибудь спокойное») ОБЯЗАТЕЛЬНО передай в функцию spotify_dj_prompt с переводом запроса на естественный английский язык в параметре prompt."
	}
	if err = c.WriteJSON(setup); err != nil {
		c.Close()
		return nil, err
	}
	var reply M
	if err = c.ReadJSON(&reply); err != nil {
		c.Close()
		return nil, err
	}
	if _, ok := reply["setupComplete"]; !ok {
		c.Close()
		return nil, fmt.Errorf("setup rejected: %v", reply["error"])
	}
	c.SetReadDeadline(time.Time{})
	return c, nil
}

func wampConn() (*websocket.Conn, error) {
	return wampConnTimeout(4 * time.Second)
}
func wampConnTimeout(timeout time.Duration) (*websocket.Conn, error) {
	d := websocket.Dialer{Subprotocols: []string{"wamp.2.json"}, HandshakeTimeout: timeout, NetDialContext: (&net.Dialer{Timeout: timeout}).DialContext}
	c, _, err := d.Dial("ws://127.0.0.1:9998/", nil)
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(1 << 20)
	c.SetReadDeadline(time.Now().Add(timeout))
	c.SetWriteDeadline(time.Now().Add(timeout))
	err = c.WriteJSON([]any{1, "default", M{"roles": M{"caller": M{}, "subscriber": M{}}}})
	if err != nil {
		c.Close()
		return nil, err
	}
	var welcome []any
	if err = c.ReadJSON(&welcome); err != nil {
		c.Close()
		return nil, err
	}
	if len(welcome) < 1 || welcome[0] != float64(2) {
		c.Close()
		return nil, errors.New("WAMP welcome rejected")
	}
	return c, nil
}
func wampCall(name string, args ...any) ([]any, error) {
	return wampCallOptions(name, M{}, args...)
}
func wampCallOptions(name string, kwargs M, args ...any) ([]any, error) {
	return wampCallTimeout(4*time.Second, name, kwargs, args...)
}
func wampCallTimeout(timeout time.Duration, name string, kwargs M, args ...any) ([]any, error) {
	c, err := wampConnTimeout(timeout)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if err = c.WriteJSON(wampCallMessage(name, args, kwargs)); err != nil {
		return nil, err
	}
	for {
		var r []any
		if err = c.ReadJSON(&r); err != nil {
			return nil, err
		}
		if len(r) > 0 && r[0] == float64(50) {
			return r, nil
		}
		if len(r) > 0 && r[0] == float64(8) {
			return nil, fmt.Errorf("native procedure %s rejected", name)
		}
	}
}
func nativeVolume() (int, error) {
	r, err := wampCall("com.harman.volumeGet")
	if err != nil {
		return 0, err
	}
	if len(r) > 4 {
		if kw, ok := r[4].(map[string]any); ok {
			if music, ok := kw["music"].(map[string]any); ok {
				if value, ok := music["volume"].(float64); ok {
					return int(value), nil
				}
			}
		}
	}
	if len(r) > 3 {
		if a, ok := r[3].([]any); ok && len(a) > 0 {
			if v, ok := a[0].(float64); ok {
				return int(v), nil
			}
		}
	}
	return 0, errors.New("unknown native volume response")
}
func tool(name string, args M) M { return toolContext(context.Background(), name, args) }
func toolContext(ctx context.Context, name string, args M) M {
	var err error
	if result, ok := handleSearchTool(ctx, name, args); ok {
		return result
	}
	if result, ok := handleTimerTool(name, args); ok {
		return result
	}
	if result, ok := handleEcosystemTool(ctx, name, args); ok {
		return result
	}
	if result, ok := handleMemoryTool(name, args); ok {
		return result
	}
	if result, ok := handleNewsTool(ctx, name, args); ok {
		return result
	}
	if result, ok := handleBroadcastTool(ctx, name, args); ok {
		return result
	}
	if result, ok := handleRoutineTool(ctx, name, args); ok {
		return result
	}
	if result, ok := handleNoteTool(name, args); ok {
		return result
	}
	switch name {
	case "get_volume":
		level, err := nativeVolume()
		if err != nil {
			return M{"ok": false, "error": clean(err)}
		}
		return M{"ok": true, "level": level, "percent": int(math.Round(float64(level) * 100 / 48))}
	case "set_volume", "adjust_volume":
		value, ok := args["level"].(float64)
		if name == "set_volume" && args["percent"] != nil {
			percent, valid := args["percent"].(float64)
			if !valid || percent < 0 || percent > 100 || math.Trunc(percent) != percent {
				return M{"ok": false, "error": "volume percent must be integer 0..100"}
			}
			value, ok = math.Round(percent*48/100), true
		}
		if name == "adjust_volume" {
			delta, valid := args["delta"].(float64)
			if !valid || delta < -6 || delta > 6 || math.Trunc(delta) != delta {
				return M{"ok": false, "error": "delta must be -6..6"}
			}
			var current int
			current, err = nativeVolume()
			if err != nil {
				return M{"ok": false, "error": clean(err)}
			}
			value = max(0, min(48, float64(current)+delta))
			ok = true
		}
		if !ok || value < 0 || value > 48 || value != float64(int(value)) {
			return M{"ok": false, "error": "volume must be integer 0..48"}
		}
		_, err = wampCall("com.harman.volumeSet", int(value))
		if err == nil {
			return M{"ok": true, "level": int(value)}
		}
	case "end_session":
		return M{"ok": true, "finish_after_reply": true, "instruction": "Give the requested final answer briefly, then end. Do not ask a follow-up question."}
	case "spotify_dj_prompt":
		prompt, _ := args["prompt"].(string)
		if strings.TrimSpace(prompt) == "" {
			return M{"ok": false, "error": "empty DJ prompt"}
		}
		err = promptSpotifyDJ(ctx, prompt)
		if err != nil {
			return M{"ok": false, "error": clean(err)}
		}
		return M{"ok": true, "prompt": prompt}
	case "media_control":
		action, _ := args["action"].(string)
		procedures := map[string]string{"pause": "com.harman.gva.pause", "resume": "com.harman.gva.resume", "next": "com.harman.gva.next", "previous": "com.harman.gva.prev", "stop": "com.harman.gva.stop"}
		p, ok := procedures[action]
		if !ok {
			return M{"ok": false, "error": "unknown media action"}
		}
		if directSpotify.connected() {
			err = directSpotify.controlPlayback(action)
			if err == nil {
				return M{"ok": true, "action": action, "backend": "spotify_api"}
			}
			if action == "resume" {
				log.Println("Spotify resume failed, attempting fallback to play_music:", err)
				request := prepareMusic(M{"music_type": "playlist", "track": ""})
				if request["ok"] == true && directSpotify.connected() {
					if resolved := directSpotify.resolve(request); resolved["ok"] == true {
						return directSpotify.playContext(ctx, resolved)
					} else {
						return resolved
					}
				}
				return M{"ok": false, "error": "Не удалось возобновить музыку: на телефоне или колонке ничего не играло."}
			}
		}
		_, err = wampCall(p)

	case "play_music":
		request := prepareMusic(args)
		if request["ok"] == true && directSpotify.connected() {
			return directSpotify.resolve(request)
		}
		if request["ok"] == true {
			return M{"ok": false, "error": "Привяжите колонку через spotify.com/pair в настройках Spotify."}
		}
		return request
	case "spotify_playback":
		action, _ := args["action"].(string)
		return directSpotify.playbackAction(ctx, action)
	case "get_current_music":
		native := readSpotifyNative()
		status := native.playback()
		var err error
		if status.State == "IDLE" {
			status, err = readPlaybackStatus()
		}
		if err != nil || status.Title == "" || (status.State != "PLAYING" && status.State != "PAUSED") {
			return M{"ok": false, "error": "Не удалось узнать текущий трек Spotify на колонке."}
		}
		result := M{"ok": true, "status": strings.ToLower(status.State), "title": status.Title, "artist": status.Artist}
		contextName, contextURI, contextType := directSpotify.playbackContext()
		if native.ContextName != "" && (strings.HasPrefix(native.ContextURI, "spotify:playlist:") || strings.HasPrefix(native.ContextURI, "spotify:album:")) {
			contextName, contextURI = native.ContextName, native.ContextURI
			contextType = "playlist"
			if strings.HasPrefix(native.ContextURI, "spotify:album:") {
				contextType = "album"
			}
			if isNativeDJContext(native.ContextURI) {
				contextType = "dj"
			}
		}
		if contextName != "" {
			result["context"] = contextName
			result["context_type"] = contextType
			result["context_uri"] = contextURI
			if contextType == "playlist" || contextType == "dj" || contextType == "mood" {
				result["playlist"] = contextName
			}
		}
		return result
	case "get_time":
		return currentTime(args)
	case "get_weather":
		return currentWeatherContext(ctx, args)
	case "set_home_city":
		return setHomeCity(args)
	default:
		return M{"ok": false, "error": "unsupported function"}
	}
	return M{"ok": err == nil, "error": clean(err)}
}

var grpcClient = &http.Client{
	Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{Timeout: 1500 * time.Millisecond}).DialContext(ctx, "unix", "/data/chrome/chirp/libassist_grpc.socket")
		},
	},
	Timeout: 3 * time.Second,
}

func grpcCall(method string, data []byte) error {
	frame := make([]byte, 5+len(data))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(data)))
	copy(frame[5:], data)
	rpcPath := "/assistant.api.ConversationService/" + method
	if strings.HasPrefix(method, "/") {
		rpcPath = method
	}
	req, err := http.NewRequest("POST", "http://localhost"+rpcPath, bytes.NewReader(frame))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/grpc")
	req.Header.Set("te", "trailers")
	res, err := grpcClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	status := res.Trailer.Get("grpc-status")
	if status == "" {
		status = res.Header.Get("grpc-status")
	}
	if status != "" && status != "0" {
		return fmt.Errorf("native %s grpc status %s", method, status)
	}
	return nil
}

type session struct {
	ctx            context.Context
	cancel         context.CancelFunc
	conn           *websocket.Conn
	write          sync.Mutex
	playerMu       sync.Mutex
	player         *mixerOutput
	mic            *nativeCapture
	activity       chan struct{}
	inputClosed    atomic.Bool
	echoGuardUntil atomic.Int64
}

func (s *session) touchActivity() {
	select {
	case s.activity <- struct{}{}:
	default:
	}
}

type controller struct {
	mu                   sync.Mutex
	active               *session
	lastError            string
	state                string
	stats                M
	connectingCancel     context.CancelFunc
	connectingGeneration uint64
}

var ctl = controller{state: "idle"}

func (c *controller) currentPhase() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// announceToActiveSession asks the current Gemini session to speak a local
// timer/alarm/reminder notification. It is best-effort: if no session is
// active, the local cue is still played by the timer manager.
func (c *controller) hasActiveSession() bool {
	c.mu.Lock()
	s := c.active
	c.mu.Unlock()
	return s != nil && s.ctx != nil && s.ctx.Err() == nil && !s.inputClosed.Load()
}

func (c *controller) announceToActiveSession(text string) {
	c.mu.Lock()
	s := c.active
	c.mu.Unlock()
	if s == nil || s.ctx == nil || s.ctx.Err() != nil || s.inputClosed.Load() {
		return
	}
	_ = s.send(M{"clientContent": M{
		"turns":        []M{{"role": "user", "parts": []M{{"text": "Служебное уведомление: " + text + " Скажи это пользователю кратко. Не вызывай end_session."}}}},
		"turnComplete": true,
	}})
}

func (s *session) send(v any) error {
	s.write.Lock()
	defer s.write.Unlock()
	s.conn.SetWriteDeadline(time.Now().Add(6 * time.Second))
	err := s.conn.WriteJSON(v)
	if err == nil {
		outboundRecording.sent(v, time.Now())
	}
	return err
}
func (s *session) stopPlayer() {
	s.finishPlayer(true)
}

// extendEchoGuard keeps microphone audio out of the cloud while our own sound
// is still in the mixer/DAC/room. The old 120 ms window was shorter than the
// acoustic tail, so the assistant transcribed its own reply.
func (s *session) extendEchoGuard(durationUs int64) {
	until := monotonicMicros() + durationUs
	for {
		old := s.echoGuardUntil.Load()
		if until <= old || s.echoGuardUntil.CompareAndSwap(old, until) {
			return
		}
	}
}
func (s *session) finishPlayer(interrupt bool) {
	s.playerMu.Lock()
	defer s.playerMu.Unlock()
	if s.player != nil {
		if interrupt {
			s.player.close()
		} else {
			if e := s.player.finish(); e != nil {
				log.Println("native mixer drain:", clean(e))
			}
		}
		s.player = nil
	}
	if interrupt {
		// A user stop/interrupt must not clip the next command for a second.
		s.extendEchoGuard(150000)
		extendAssistantAudioGuard(300 * time.Millisecond)
	} else {
		// Keep the room-tail guard short. The old 600 ms guard forced the
		// follow-up path to recover pre-roll audio that still contained the
		// assistant's own TTS tail; ASR then heard "Citation" instead of the
		// user's short answer.
		s.extendEchoGuard(300000)
		extendAssistantAudioGuard(3 * time.Second)
	}
}
func (s *session) play(data []byte) error {
	s.playerMu.Lock()
	defer s.playerMu.Unlock()
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	if s.player == nil {
		player, e := openMixerOutput()
		if e != nil {
			return e
		}
		s.player = player
	}
	// 24 kHz mono S16 plus 300 ms for the mixer/DAC tail. This also covers
	// cue playback that happens while the session is still "listening".
	if len(data) > 0 {
		durationUs := int64(len(data)) * 1000000 / (24000 * 2)
		s.extendEchoGuard(durationUs + 300000)
		// Keep the native hotword detector from waking on our own voice.
		extendAssistantAudioGuard(time.Duration(durationUs)*time.Microsecond + 1500*time.Millisecond)
	}
	return s.player.write(data)
}
func led(name string) {
	states := map[string]string{"002_listening": "listening", "003_thinking": "thinking", "004_responding": "speaking"}
	if state, ok := states[name]; ok {
		_, _ = wampCall("com.harman.ledAnimate", "animation_close")
		_, err := wampCallOptions("com.harman.ledAnimate", M{"repeat": true}, "../../../data/opt/citation-gemini/led/"+state)
		if err == nil {
			return
		}
	}
	_, _ = wampCall("com.harman.ledAnimate", name)
}
func (c *controller) stop() {
	c.mu.Lock()
	if c.connectingCancel != nil {
		c.connectingCancel()
	}
	s := c.active
	if s != nil {
		s.cancel()
		s.conn.Close()
	}
	c.mu.Unlock()
	nativeHoldStart.Store(false)
}
func (c *controller) start() { c.startAfterNativeWake(nil) }
func (c *controller) startFromNativeHold() {
	// Light Gemini's listening ring immediately on hold so AudioUI leaves
	// assistant-idle with transition="" instead of defaulting to 001_hotword.
	_ = applyNativePhase("listening")
	nativeStopped := make(chan struct{})
	go func() {
		// AudioUI dispatches gva.triggerAssistant asynchronously on btn-action-long;
		// cast_shell / google-wamp can start SpeechRecognitionActivity anywhere from
		// ~45 ms to ~140 ms later. Cancel at fixed offsets across the fast window
		// before closing nativeStopped, then guard the late tail while active.
		started := time.Now()
		stopNativeInteraction()
		for _, offset := range []time.Duration{
			8 * time.Millisecond,
			18 * time.Millisecond,
			28 * time.Millisecond,
			40 * time.Millisecond,
			55 * time.Millisecond,
			72 * time.Millisecond,
		} {
			if rem := offset - time.Since(started); rem > 0 {
				time.Sleep(rem)
			}
			stopNativeInteraction()
		}
		close(nativeStopped)
		for _, offset := range []time.Duration{
			95 * time.Millisecond,
			125 * time.Millisecond,
			160 * time.Millisecond,
			200 * time.Millisecond,
		} {
			if rem := offset - time.Since(started); rem > 0 {
				time.Sleep(rem)
			}
			if !c.isListeningOrConnecting() {
				return
			}
			stopNativeInteraction()
		}
		_ = grpcCall("StopAllTts", nil)
	}()
	nativeHoldStart.Store(true)
	c.startAfterNativeWake(nativeStopped)
}
func (c *controller) waitForIdle(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		idle := c.active == nil && c.state == "idle"
		c.mu.Unlock()
		if idle {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func (c *controller) startAfterNativeWake(nativeStopped <-chan struct{}) {
	if nativeStopped != nil && !c.waitForIdle(2*time.Second) {
		log.Println("native wake dropped: previous session did not close in time")
		nativeHoldStart.Store(false)
		return
	}
	resetVoiceQueryBuffer()
	c.mu.Lock()
	if c.active != nil || c.state != "idle" {
		c.mu.Unlock()
		nativeHoldStart.Store(false)
		return
	}
	c.state = "connecting"
	c.lastError = ""
	connectingCtx, connectingCancel := context.WithCancel(context.Background())
	c.connectingGeneration++
	generation := c.connectingGeneration
	c.connectingCancel = connectingCancel
	c.mu.Unlock()
	defer func() {
		connectingCancel()
		c.mu.Lock()
		if c.connectingGeneration == generation {
			c.connectingCancel = nil
		}
		c.mu.Unlock()
	}()
	// Subscribe before any mixer/RPC work. Wake-event delivery can lag actual
	// speech, so the bounded local pre-roll also preserves the command onset.
	earlyMic, earlyError := openBufferedNativeCapture()
	if earlyError != nil {
		_ = ducking.Restore()
		c.mu.Lock()
		c.state = "idle"
		c.lastError = "microphone start failed"
		c.mu.Unlock()
		c.setPhase("idle")
		nativeHoldStart.Store(false)
		return
	}
	adoptedMic := false
	defer func() {
		if !adoptedMic {
			earlyMic.close()
			_ = ducking.Restore()
		}
	}()
	if nativeHoldStart.Load() && earlyMic.wakeBoundaryUs != 0 {
		// A touch hold has no spoken command before the cue. Keep only a short
		// onset; the full hardware-wake pre-roll is room audio or music from
		// before the request and must not become part of the command.
		earlyMic.uploadAfterUs = earlyMic.wakeBoundaryUs - 300000
	}
	// Overlap the cloud handshake with local ducking, Google cancellation and
	// LED setup. Capture is already subscribed, so no command audio is lost.
	musicAtWake := nativeMusicPlayingFast()
	setupStarted := time.Now()
	type cloudConnectionResult struct {
		conn *websocket.Conn
		err  error
	}
	cloudReady := make(chan cloudConnectionResult)
	go func() {
		conn, err := cloudContextActivity(connectingCtx, usingNativeCaptureService(), musicAtWake)
		select {
		case cloudReady <- cloudConnectionResult{conn, err}:
		case <-connectingCtx.Done():
			if conn != nil {
				conn.Close()
			}
		}
	}()
	var duckSettledUs int64
	if ducking.DuckListening() == nil {
		duckSettledUs = monotonicMicros() + 150000
	}
	// Capture is already subscribed, so a continuous wake+command is retained.
	// Publish our UI only after Google's StopInteraction finished publishing idle.
	if nativeStopped != nil {
		select {
		case <-nativeStopped:
			waitNativeWakeCancellation(connectingCtx)
		case <-connectingCtx.Done():
			c.setPhase("idle")
			return
		}
	}
	if cfg.ReplaceGoogle {
		if silencer, e := openTTSRedirect(); e == nil {
			defer silencer.close()
		} else {
			log.Println("native voice guard:", clean(e))
		}
	}
	resetWakeSFXTracking()
	go playWakeSFX(isRecentDJWake())
	if !cfg.NativeBehavior {
		go playRandomCue()
	}
	c.setPhase("listening")
	var conn *websocket.Conn
	var err error
	select {
	case result := <-cloudReady:
		conn, err = result.conn, result.err
	case <-connectingCtx.Done():
		c.setPhase("idle")
		return
	}
	log.Printf("Gemini setup including local preparation: %s", time.Since(setupStarted))
	if err != nil {
		c.mu.Lock()
		c.state = "idle"
		if connectingCtx.Err() == nil {
			c.lastError = clean(err)
		}
		c.mu.Unlock()
		c.setPhase("idle")
		log.Println("session setup:", clean(err))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	s := &session{ctx: ctx, cancel: cancel, conn: conn, activity: make(chan struct{}, 1)}
	c.mu.Lock()
	if connectingCtx.Err() != nil {
		c.mu.Unlock()
		cancel()
		conn.Close()
		c.setPhase("idle")
		return
	}
	c.connectingCancel = nil
	c.active = s
	// Keep the already-published listening phase across the cloud handshake.
	voiceName := cfg.Voice
	if voiceName == "" {
		voiceName = "Charon"
	}
	c.stats = M{"voice": voiceName, "server_events": 0, "session_limit_seconds": 900}
	if cfg.TestTrace {
		c.stats["test_buffered_frames_at_setup"] = len(earlyMic.frames)
	}
	c.mu.Unlock()
	var processor *aecPipeline
	micDone := make(chan struct{})
	micStarted := false
	var musicSelection M
	var musicLaunch chan M
	var releaseMusicSilence func() error
	var mediaStopped bool
	var musicStarted bool
	var awaitingToolResponse atomic.Bool
	var finalReplyDeadline atomic.Int64
	var lastReplyAudioAt atomic.Int64
	var nativeInputRevision atomic.Uint64
	var nativeInputActive atomic.Bool
	var meaningfulInput atomic.Bool
	// nativeAllowStart disarms the native VAD gate between turns. Noise must
	// not be able to open a new turn on its own; only a wake boundary or an
	// explicit follow-up question re-arms it.
	var nativeAllowStart atomic.Bool
	// lastMeaningfulAt is the wall-clock time of the last transcript accepted
	// by requestGuard. It backs the no-meaningful-input watchdog.
	var lastMeaningfulAt atomic.Int64
	// Follow-up window: after a reply finishes, keep listening this long.
	var followUpUntil atomic.Int64
	// armFollowUp waits for the assistant's room tail/echo guard to expire
	// before arming the microphone. Opening the window at turnComplete let the
	// mic capture the last syllable of our own TTS as a follow-up.
	armFollowUp := func(window time.Duration) {
		delay := followUpDelay(monotonicMicros(), s.echoGuardUntil.Load())
		nativeAllowStart.Store(false)
		until := time.Now().Add(delay + window).UnixNano()
		followUpUntil.Store(until)
		go func() {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			for {
				select {
				case <-timer.C:
					if s.ctx.Err() != nil || followUpUntil.Load() != until {
						return
					}
					if remaining := s.echoGuardUntil.Load() - monotonicMicros(); remaining > 0 {
						timer.Reset(time.Duration(remaining)*time.Microsecond + 100*time.Millisecond)
						continue
					}
					followUpUntil.Store(time.Now().Add(window).UnixNano())
					markFollowupGate(window)
					c.setPhase("listening")
					nativeAllowStart.Store(true)
					log.Printf("Follow-up window armed after echo guard (%s)", delay)
					return
				case <-s.ctx.Done():
					return
				}
			}
		}()
	}
	var nativeAwaitingResult atomic.Bool
	var postWakeSpeech atomic.Bool
	var isInitialWake atomic.Bool
	isInitialWake.Store(true)
	closeInput := func() {
		s.inputClosed.Store(true)
		if s.mic != nil {
			s.mic.close()
			s.mic = nil
		}
		if micStarted {
			<-micDone
			micStarted = false
		}
		if processor != nil {
			stats := processor.close()
			c.mu.Lock()
			for name, value := range stats {
				c.stats[name] = value
			}
			c.mu.Unlock()
			processor = nil
		}
	}
	defer func() {
		cancel()
		conn.Close()
		s.stopPlayer()
		closeInput()
		if musicLaunch != nil {
			// The session ended before the announcement completed. Do not
			// release a prepared track onto the speaker after cancellation.
			// Bound the wait: a stuck Spotify call must not keep the
			// controller busy and leak the microphone/duck state forever.
			select {
			case result := <-musicLaunch:
				if result["ok"] == true {
					_, _ = wampCall("com.harman.gva.pause")
				}
			case <-time.After(2 * time.Second):
				log.Println("Spotify launch did not finish during session cleanup")
			}
		}
		if releaseMusicSilence != nil {
			_ = releaseMusicSilence()
		}
		_ = ducking.Restore()
		audioContentPlaying := (musicAtWake && !mediaStopped) || musicStarted || musicSelection != nil || (!mediaStopped && nativeMusicPlaying())
		if !audioContentPlaying {
			go playEndSessionSFX()
		}
		c.setPhase("idle")
		c.mu.Lock()
		c.active = nil
		c.state = "idle"
		if ctx.Err() == context.DeadlineExceeded {
			c.lastError = "test session time limit reached (15 minutes)"
		}
		c.mu.Unlock()
		log.Println("Gemini session closed:", ctx.Err())
	}()
	go func() { <-ctx.Done(); conn.Close() }()
	go func() {
		timer := time.NewTimer(8 * time.Second)
		defer timer.Stop()
		awaitingTicks := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if deadline := finalReplyDeadline.Load(); deadline != 0 && time.Now().UnixNano() >= deadline {
					log.Println("Final reply did not arrive before deadline; closing session")
					cancel()
					return
				}
				if rest := followUpUntil.Load() - time.Now().UnixNano(); rest > 0 {
					timer.Reset(time.Duration(rest))
					continue
				}
				phase := c.currentPhase()
				if last := lastReplyAudioAt.Load(); last != 0 && replyAudioStalled(phase, time.Unix(0, last), time.Now(), monotonicMicros() < s.echoGuardUntil.Load()) {
					log.Println("Gemini reply stream stalled for 8s after playback; closing session")
					c.mu.Lock()
					c.lastError = "Gemini reply stream stalled after partial audio"
					c.mu.Unlock()
					cancel()
					return
				}
				// Only an accepted transcript is real user activity. An open
				// microphone, postWakeSpeech or a noise endpoint is not.
				if meaningfulSessionActivity(meaningfulInput.Load(), postWakeSpeech.Load(), nativeInputActive.Load(), awaitingToolResponse.Load(), phase) {
					awaitingTicks = 0
					timer.Reset(5 * time.Second)
					continue
				}
				// Backstop for sessions that produced a real request and then
				// went quiet: do not let a stale follow-up/tool state keep the
				// socket open indefinitely.
				if last := lastMeaningfulAt.Load(); last != 0 && time.Since(time.Unix(0, last)) >= 12*time.Second {
					log.Println("No meaningful input for 12s; closing session")
					cancel()
					return
				}
				// Wait for a late transcript after a speech endpoint, but noise-only
				// endpoints (no real speech) get at most one short grace period.
				maxTicks := 1
				if meaningfulInput.Load() {
					maxTicks = 3
				}
				if nativeAwaitingResult.Load() && awaitingTicks < maxTicks {
					awaitingTicks++
					timer.Reset(3 * time.Second)
					continue
				}
				log.Println("Voice idle timeout reached; closing session")
				cancel()
				return
			case <-s.activity:
				awaitingTicks = 0
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(8 * time.Second)
			}
		}
	}()
	processor, err = openAecPipeline()
	if err != nil {
		c.mu.Lock()
		c.lastError = "native AEC unavailable: " + clean(err)
		c.mu.Unlock()
		return
	}
	mic := earlyMic
	adoptedMic = true
	s.mic = mic
	micStarted = true
	go func() {
		defer close(micDone)
		var activityGate *nativeActivityGate
		var activityPreRoll [][]byte
		var audioBatch nativeAudioBatch
		sendActivityEnd := func() bool {
			if err := s.send(M{"realtimeInput": M{"activityEnd": M{}}}); err != nil {
				cancel()
				return false
			}
			nativeInputActive.Store(false)
			nativeAwaitingResult.Store(true)
			nativeAllowStart.Store(false)
			// Only an accepted transcript proves a real user turn. Post-wake
			// VAD evidence alone is noise-sensitive and must not extend the
			// session.
			if meaningfulInput.Load() {
				s.touchActivity()
				if c.currentPhase() == "listening" {
					c.setPhase("thinking")
				}
			}
			log.Printf("Native speech endpoint; request sent (postWakeSpeech=%v)", postWakeSpeech.Load())
			return true
		}
		if usingNativeCaptureService() {
			activityGate = newNativeActivityGate()
			// Nothing before the wake boundary may open a turn. The wake word
			// and the wake cue are pre-roll only.
			activityGate.allowStart = false
		}
		frames := 0
		var nearEnergy, cleanEnergy float64
		var nearPeak, cleanPeak, sessionCleanPeakMax int
		var uploadedPeak, sessionUploadPeakMax, clippedSamples int
		var uploadedSpeechFrames, droppedNoiseFrames int
		var lastSpeechFrameUs int64
		// Upload speech plus a short hangover so the end of a word is not cut.
		// Non-speech frames outside the hangover are dropped instead of being
		// uploaded as background noise.
		const speechHangoverUs int64 = 250000
		var quietGain quietSpeechGain
		var stockGain *stockDynamicGain
		if !usingNativeCaptureService() && stockDynamicGainPolicyEnabled() {
			stockCfg := stockDynamicGainConfig()
			stockGain = newStockDynamicGain(stockCfg, stockCfg.MaxSignalGain)
		}
		var lastStockGain, lastStockSNR float64
		var dcFilter microphoneDCFilter
		baseGain := microphoneGain()
		if usingNativeCaptureService() {
			// The processed observer is already the firmware AEC/NS/gain path.
			// Do not stack a calibrated boost or limiter on top of it.
			baseGain = 1
		}
		lastAppliedGain := baseGain
		maxAppliedGain := baseGain
		postWakeFrames := 0
		wakeBoundaryUs := earlyMic.wakeBoundaryUs
		if isRecentDJWake() {
			wakeBoundaryUs = earlyMic.uploadAfterUs
		}
		wakeBoundaryPassed := false
		preRollStarted := false
		wasInEchoGuard := false
		followUpEchoRecoveries := 0
		for {
			frame, timestamp, err := mic.read()
			if err != nil {
				if !s.inputClosed.Load() {
					cancel()
				}
				return
			}
			phase := c.currentPhase()
			nowUs := monotonicMicros()
			// A follow-up window is a bounded permission to open a new turn.
			// Once it expires, disarm the gate until the next wake.
			if until := followUpUntil.Load(); until != 0 && time.Now().UnixNano() > until && !nativeInputActive.Load() {
				nativeAllowStart.Store(false)
				followUpUntil.Store(0)
				clearFollowupGate()
				log.Println("Follow-up window expired; disarming microphone")
			}
			// Do not shorten a guard here: finishPlayer sets the post-reply
			// tail, and a 120 ms Store could overwrite it while the phase is
			// still "speaking". Playback itself extends the guard in play().
			guardUntil := s.echoGuardUntil.Load()
			inEchoGuard := nowUs < guardUntil || (timestamp > 0 && timestamp < guardUntil)
			if activityGate != nil && wasInEchoGuard && !inEchoGuard && nativeAllowStart.Load() && followUpUntil.Load() != 0 && !activityGate.active {
				// The user often starts the follow-up while the assistant's
				// room tail is still inside the echo guard. The pre-roll was
				// collected during the guard; recover it instead of losing the
				// first word.
				if activityGate.preRollHasVoice(activityPreRoll) {
					activityGate.forceStart()
					preRollStarted = true
					followUpEchoRecoveries++
					log.Println("Follow-up speech recovered from echo-guard pre-roll")
				}
			}
			wasInEchoGuard = inEchoGuard
			// Wake/random cue audio is played outside session.play. Buffered
			// pre-roll is processed after the cue has already finished, so use
			// its timestamp window rather than a short real-time flag.
			inWakeCueGuard := false
			if cueStart, cueEnd := lastWakeSFXStartUs.Load(), lastWakeSFXEndUs.Load(); cueStart != 0 && cueEnd > cueStart {
				ts := timestamp
				if ts == 0 {
					ts = nowUs
				}
				if ts >= cueStart && ts <= cueEnd {
					inEchoGuard = true
					inWakeCueGuard = true
				}
			}
			listening := phase == "listening" && !inEchoGuard
			processor.listening.Store(listening)
			filtered, e := processor.process(frame, timestamp)
			if e != nil {
				c.mu.Lock()
				c.lastError = "AEC processing failed: " + clean(e)
				c.mu.Unlock()
				cancel()
				return
			}
			if activityGate != nil && !usingNativeCaptureService() {
				dcFilter.apply(filtered)
			}
			if localCuePlaying.Load() || inWakeCueGuard {
				// Never upload our own wake cue as a user command. The cue is
				// played to confirm activation; the model must not answer it.
				continue
			}
			// Warm the native AEC on the older capture/reference history, but
			// send the selected pre-roll and the actual live command.
			if mic.uploadAfterUs != 0 && timestamp < mic.uploadAfterUs {
				if activityGate != nil && !musicAtWake {
					activityGate.warm(filtered)
				}
				continue
			}
			var activity nativeActivityEvent
			if activityGate != nil {
				if !wakeBoundaryPassed && (wakeBoundaryUs == 0 || timestamp >= wakeBoundaryUs) {
					wakeBoundaryPassed = true
					activityGate.markWakeBoundary()
					// The wake boundary re-arms the gate for this session.
					nativeAllowStart.Store(true)
					// A quick "Hey Google, <command>" can be entirely in the
					// pre-roll. Submit it now instead of waiting for live
					// speech that may never come. A touch-panel hold has no
					// command before the cue, so its pre-roll is silence or
					// our own cue and must not open a turn.
					touchStart := nativeHoldStart.Swap(false)
					if len(activityPreRoll) > 0 && !touchStart {
						activityGate.forceStart()
						// Wake-only pre-roll must leave time for the actual command.
						activityGate.minEnd = activityGate.frames + 150
						preRollStarted = true
					}
				}
				if wakeBoundaryPassed && duckSettledUs != 0 && timestamp >= duckSettledUs && listening && !nativeAwaitingResult.Load() && !activityGate.active {
					activityGate.observeDuckedBackground(filtered)
				}
				activityPhase := phase
				// The short wake cue is allowed through: the user often starts
				// speaking as soon as the LED turns blue, and cutting that
				// window makes the first words disappear. Post-reply TTS echo
				// still gets the playback-tail phase.
				if phase == "listening" && inEchoGuard && !inWakeCueGuard {
					activityPhase = "playback-tail"
				}
				if phase == "listening" && nativeAwaitingResult.Load() {
					activityPhase = "awaiting-transcript"
				}
				// The gate owns waitingForCommand. Re-arming it here reset the
				// state on every frame and kept short false starts open forever.
				// Re-arm only from session policy: the wake boundary or an
				// explicit follow-up question.
				activityGate.allowStart = nativeAllowStart.Load()
				activity = activityGate.observe(filtered, activityPhase)
				if preRollStarted {
					if !activity.end {
						activity.start = true
						activity.audio = true
						// Pre-roll speech is not live post-wake evidence. Do not
						// let the wake word alone authorise a model response.
						activity.speech = false
					}
					preRollStarted = false
				}
				if activity.start {
					postWakeFrames = 0
					postWakeSpeech.Store(false)
				}
				// Immediately track user speech after hotword boundary.
				commandBoundaryUs := wakeBoundaryUs
				if activity.audio && activity.speech && timestamp >= commandBoundaryUs && listening {
					postWakeFrames++
					if postWakeFrames >= 5 {
						postWakeSpeech.Store(true)
					}
				} else if postWakeFrames > 0 {
					postWakeFrames--
				}
				// Never let one turn's AGC power average leak into the next.
				if activity.start || activity.end {
					stockGain.reset()
				}
			}
			framePeak := 0
			for i := 0; i < len(frame); i += 2 {
				n := float64(int16(binary.LittleEndian.Uint16(frame[i:])))
				v := float64(int16(binary.LittleEndian.Uint16(filtered[i:])))
				nearEnergy += n * n
				cleanEnergy += v * v
				if int(math.Abs(n)) > nearPeak {
					nearPeak = int(math.Abs(n))
				}
				if int(math.Abs(v)) > framePeak {
					framePeak = int(math.Abs(v))
				}
				if int(math.Abs(v)) > cleanPeak {
					cleanPeak = int(math.Abs(v))
				}
				if int(math.Abs(v)) > sessionCleanPeakMax {
					sessionCleanPeakMax = int(math.Abs(v))
				}
			}
			frames++
			// Confirm speech with both the VAD and a minimum level relative to
			// the learned room floor. The gate stays conservative: quiet_rms is
			// ~8.19 and noise_floor ~5.17, so max(6.5, floor*1.3) ~= 6.7 still
			// admits the user's quiet voice while rejecting the room floor.
			gainRMS := nativeFrameACRMS(filtered)
			minGainRMS := 6.5
			speechForGain := activity.speech && listening && gainRMS >= minGainRMS
			var appliedGain float64
			appliedGain = baseGain
			if !usingNativeCaptureService() {
				appliedGain = quietGain.multiplier(filtered, speechForGain, baseGain, framePeak)
			}
			// The stock libassistant AutoGainControl normalises confirmed
			// speech to a fixed target and clamps the result to the recovered
			// min/max range. Keep the existing quiet-speech gain as a floor:
			// the stock policy may boost more, but must never make the uplink
			// quieter than the already validated path.
			if stockGain != nil && speechForGain {
				// Without a calibrated profile we cannot distinguish a
				// quiet voice from room noise, so use the signal ceiling.
				// The noise ceiling is only selected when a real SNR
				// measurement is below the stock max_snr threshold.
				snrDB := stockGain.cfg.MaxSNR
				stock := stockGain.gainForPCM(filtered, snrDB)
				// Cap the boost by measured SNR: a low-SNR frame is probably
				// noise and must not receive the full quiet-speech boost.
				gainCap := 4.0
				if snrDB >= 12 {
					gainCap = 16
				} else if snrDB >= 6 {
					gainCap = 8
				}
				if stock > gainCap {
					stock = gainCap
				}
				// Absolute final safety, even if the config changes.
				if stock > 16 {
					stock = 16
				}
				if stock > appliedGain {
					appliedGain = stock
				}
				lastStockGain, lastStockSNR = stock, snrDB
			}
			// Keep the full wake pre-roll, but don't boost the playback floor
			// before wake. Speech and all post-wake frames keep normal gain.
			if !usingNativeCaptureService() && activityGate != nil && musicAtWake && earlyMic.uploadAfterUs != 0 && timestamp < wakeBoundaryUs && !activity.speech {
				appliedGain *= 0.125
			}
			startGain := appliedGain
			if activityGate != nil && !usingNativeCaptureService() {
				// Ramp across the frame, but reduce immediately if the new
				// frame's peak would overload the signal at the old gain.
				safeGain := 23170 / math.Max(1, float64(framePeak))
				startGain = math.Min(lastAppliedGain, safeGain)
				appliedGain = math.Min(appliedGain, safeGain)
				lastAppliedGain = appliedGain
			}
			maxAppliedGain = math.Max(maxAppliedGain, math.Max(startGain, appliedGain))
			peak, clipped := gainPCM16Ramp(filtered, startGain, appliedGain)
			if peak > uploadedPeak {
				uploadedPeak = peak
			}
			if peak > sessionUploadPeakMax {
				sessionUploadPeakMax = peak
			}
			clippedSamples += clipped
			// During the first wake/command, VAD evidence keeps the session
			// alive while ASR catches up. After that, only a meaningful
			// transcript or model audio may keep it alive; otherwise TV/noise
			// extends the session forever.
			if activity.speech && postWakeSpeech.Load() && (isInitialWake.Load() || meaningfulInput.Load()) {
				s.touchActivity()
			}
			if activityGate == nil && framePeak > 500 && frames%20 == 0 {
				s.touchActivity()
			}
			if frames%100 == 0 {
				c.mu.Lock()
				c.stats["microphone_rms"] = math.Sqrt(nearEnergy / 16000)
				c.stats["clean_microphone_rms"] = math.Sqrt(cleanEnergy / 16000)
				c.stats["microphone_peak"] = nearPeak
				c.stats["clean_microphone_peak"] = cleanPeak
				c.stats["session_clean_peak_max"] = sessionCleanPeakMax
				c.stats["capture_age_ms"] = (monotonicMicros() - timestamp) / 1000
				c.stats["captured_seconds"] = frames / 100
				c.stats["microphone_gain"] = baseGain
				c.stats["microphone_gain_applied_max"] = maxAppliedGain
				if stockGain != nil {
					c.stats["stock_dynamic_gain"] = math.Round(lastStockGain*1000) / 1000
					c.stats["stock_dynamic_gain_db"] = math.Round(stockDynamicGainDB(lastStockGain)*100) / 100
					c.stats["stock_dynamic_gain_snr_db"] = math.Round(lastStockSNR*10) / 10
				}
				c.stats["upload_peak"] = uploadedPeak
				c.stats["session_upload_peak_max"] = sessionUploadPeakMax
				c.stats["clipped_samples"] = clippedSamples
				c.stats["post_wake_speech_evidence"] = postWakeSpeech.Load()
				c.stats["uploaded_speech_frames"] = uploadedSpeechFrames
				c.stats["dropped_noise_frames"] = droppedNoiseFrames
				c.stats["followup_echo_recoveries"] = followUpEchoRecoveries
				if activityGate != nil {
					c.stats["gate_threshold"] = math.Round(activityGate.threshold*100) / 100
					gateRMS, gateZCR, gateLow := activityGate.lastFeatures()
					c.stats["last_gate_rms"] = math.Round(gateRMS*100) / 100
					c.stats["last_gate_zcr"] = math.Round(gateZCR*1000) / 1000
					c.stats["last_gate_low_ratio"] = math.Round(gateLow*1000) / 1000
				}
				c.mu.Unlock()
				nearEnergy = 0
				cleanEnergy = 0
				nearPeak = 0
				cleanPeak = 0
				uploadedPeak = 0
			}
			if activityGate != nil {
				if activity.end && !activity.audio {
					// A phase transition can close an open activity without a
					// final audio frame. Flush any buffered speech, then send
					// the matching activityEnd instead of dropping the event in
					// the !audio branch below.
					if pending := audioBatch.append(nil, true); len(pending) > 0 {
						if err := s.send(M{"realtimeInput": M{"audio": M{"mimeType": "audio/pcm;rate=16000", "data": base64.StdEncoding.EncodeToString(pending)}}}); err != nil {
							if ctx.Err() == nil {
								c.mu.Lock()
								c.lastError = "microphone upload failed: " + clean(err)
								c.mu.Unlock()
							}
							cancel()
							return
						}
					}
					if !sendActivityEnd() {
						return
					}
					activityPreRoll = nil
					lastSpeechFrameUs = 0
					continue
				}
				if !activity.audio {
					// Only accumulate a pre-roll while the gate may start a
					// turn (before the wake boundary, or inside a follow-up
					// window). Otherwise keyboard noise would build a buffer
					// that gets uploaded with the next real turn.
					if phase == "listening" && (listening || inWakeCueGuard) && (nativeAllowStart.Load() || !wakeBoundaryPassed) {
						activityPreRoll = append(activityPreRoll, filtered)
						// Keep 400 ms of wake pre-roll. A continuous
						// "Hey Google, <command>" can start well before the
						// wake event is delivered to this process.
						limit := 40
						if !wakeBoundaryPassed {
							limit = 300
						}
						if len(activityPreRoll) > limit {
							activityPreRoll = activityPreRoll[len(activityPreRoll)-limit:]
						}
					} else {
						activityPreRoll = nil
					}
					continue
				}
				if activity.start {
					resetVoiceQueryBuffer()
					meaningfulInput.Store(false)
					nativeInputRevision.Add(1)
					nativeInputActive.Store(true)
					// A short follow-up word can start just before the window
					// expires. Extend it while the user is speaking so the tail
					// is not cut off.
					if until := followUpUntil.Load(); until != 0 {
						followUpUntil.Store(time.Now().Add(2 * time.Second).UnixNano())
						markFollowupGate(2 * time.Second)
						log.Println("Follow-up speech start; extending follow-up window")
					}
					// A gate start is not proof of human speech. postWakeSpeech
					// is set later from confirmed voice frames only.
					if err := s.send(M{"realtimeInput": M{"activityStart": M{}}}); err != nil {
						cancel()
						return
					}
					if phase != "listening" {
						c.setPhase("listening")
					}
					if len(activityPreRoll) > 0 {
						prefix := make([]byte, 0, (len(activityPreRoll)+1)*320)
						for _, old := range activityPreRoll {
							prefix = append(prefix, old...)
						}
						filtered = append(prefix, filtered...)
						activityPreRoll = nil
					}
				}
			}
			if activityGate != nil {
				// Upload only confirmed speech, a short hangover after the
				// last speech frame, and the activity boundaries needed to
				// flush the stream. Everything else is background noise and
				// must not reach the cloud.
				upload := activity.audio || activity.start || activity.end
				if !upload && lastSpeechFrameUs != 0 && nowUs-lastSpeechFrameUs <= speechHangoverUs {
					upload = true
				}
				if !upload {
					droppedNoiseFrames++
					continue
				}
				if activity.speech || activity.start {
					lastSpeechFrameUs = nowUs
				}
				if activity.speech {
					uploadedSpeechFrames++
				}
				appendVoiceQueryFrame(filtered)
				filtered = audioBatch.append(filtered, activity.end)
				if len(filtered) == 0 {
					continue
				}
			}
			if err := s.send(M{"realtimeInput": M{"audio": M{"mimeType": "audio/pcm;rate=16000", "data": base64.StdEncoding.EncodeToString(filtered)}}}); err != nil {
				if ctx.Err() == nil {
					c.mu.Lock()
					c.lastError = "microphone upload failed: " + clean(err)
					c.mu.Unlock()
				}
				cancel()
				return
			}
			if activity.end {
				if !sendActivityEnd() {
					return
				}
			}
		}
	}()
	log.Println("Gemini session active; firmware WebRTC AEC, native Cast mixer and loopback")
	c.setPhase("listening")
	var commands commandGate
	commands.input()
	voiceSignals := false
	inputText, outputText := "", ""
	lastAssistantOutput := ""
	recognitionAction := "no_action"
	var recognitionText strings.Builder
	defer func() { rememberRecognition(recognitionText.String(), recognitionAction) }()
	endAfterTurn := false
	replyAudioSinceInput := false
	var rememberedRevision uint64
	rememberReply := func(reply string) {
		if commands.inputRevision != rememberedRevision && strings.TrimSpace(inputText) != "" && strings.TrimSpace(reply) != "" {
			recentConversation.remember(inputText, reply, time.Now())
			rememberedRevision = commands.inputRevision
		}
	}
	responding := false
	var requestGuard wakeRequestGuard
	// Stock libassistant rate-limits scheduled mic reopens. Keep a high ceiling
	// so normal follow-ups are unaffected but a self-echo/reply loop cannot
	// reopen the microphone indefinitely.
	micReopen := newMicReopenPolicy(5, 5*time.Second)
	allowTurnResponse := func() bool {
		// Only a meaningful transcript in the user's language authorises a
		// reply. VAD alone is not evidence: TV, other people and our own TTS
		// all look like speech. Model audio is queued until this returns true.
		return meaningfulInput.Load() || requestGuard.allowResponse()
	}
	var pendingToolCalls []any
	pendingTurnComplete := false
	playReplyAudio := func(audio []byte) error {
		if len(audio) == 0 {
			return nil
		}
		lastReplyAudioAt.Store(time.Now().UnixNano())
		replyAudioSinceInput = true
		finalReplyDeadline.Store(0)
		awaitingToolResponse.Store(false)
		s.touchActivity()
		if !responding {
			c.setPhase("speaking")
			responding = true
			log.Printf("Gemini reply playback started")
		}
		if e := s.play(audio); e != nil {
			return e
		}
		c.mu.Lock()
		count, _ := c.stats["assistant_audio_bytes"].(int)
		c.stats["assistant_audio_bytes"] = count + len(audio)
		c.mu.Unlock()
		return nil
	}
	var observedNativeInputRevision uint64
	toolResults := map[string]M{}
	for {
		var data M
		if err = conn.ReadJSON(&data); err != nil {
			if ctx.Err() == nil {
				log.Println("Gemini receive failed:", clean(err))
				c.mu.Lock()
				c.lastError = clean(err)
				c.mu.Unlock()
			}
			return
		}
		if e, ok := data["error"]; ok {
			c.mu.Lock()
			c.lastError = clean(fmt.Errorf("%v", e))
			c.mu.Unlock()
			return
		}
		if revision := nativeInputRevision.Load(); revision != observedNativeInputRevision {
			observedNativeInputRevision = revision
			if !awaitingToolResponse.Load() && data["toolCall"] == nil {
				replyAudioSinceInput = false
				inputText, outputText = "", ""
				requestGuard.reset()
				pendingToolCalls = nil
				pendingTurnComplete = false
				toolResults = map[string]M{}
				if cfg.TestTrace {
					c.mu.Lock()
					delete(c.stats, "test_input_transcript")
					delete(c.stats, "test_output_transcript")
					c.mu.Unlock()
				}
			}
			commands.end()
			commands.input()
		}
		c.mu.Lock()
		c.stats["server_events"] = c.stats["server_events"].(int) + 1
		c.stats["last_server_event_unix"] = time.Now().Unix()
		c.mu.Unlock()
		if activity, ok := data["voiceActivity"].(map[string]any); ok {
			// Native capture owns VAD; a cloud voice-activity event is not
			// proof of a meaningful user turn.
			if !usingNativeCaptureService() || meaningfulInput.Load() {
				s.touchActivity()
			}
			voiceSignals = true
			// Manual capture already owns the phase and input revision.
			// Delayed cloud notifications can refer to the preceding request
			// while its reply is playing.
			if !usingNativeCaptureService() && activity["type"] == "ACTIVITY_START" {
				replyAudioSinceInput = false
				if !s.inputClosed.Load() {
					c.setPhase("listening")
				}
				inputText = ""
				outputText = ""
				commands.input()
			}
			if !usingNativeCaptureService() && activity["type"] == "ACTIVITY_END" {
				if !s.inputClosed.Load() && meaningfulInput.Load() {
					c.setPhase("thinking")
				}
				commands.end()
			}
			c.mu.Lock()
			c.stats["voice_activity_supported"] = true
			c.stats["input_utterances"] = commands.inputRevision
			c.mu.Unlock()
		}
		if content, ok := data["serverContent"].(map[string]any); ok {
			if transcript, ok := content["outputTranscription"].(map[string]any); ok {
				text, _ := transcript["text"].(string)
				outputText += text
				if cfg.TestTrace {
					c.mu.Lock()
					c.stats["test_output_transcript"] = outputText
					c.mu.Unlock()
				}
			}
			if transcript, ok := content["inputTranscription"].(map[string]any); ok {
				commands.input()
				if text, _ := transcript["text"].(string); strings.TrimSpace(text) != "" {
					inputText += text
					echoTranscript := isLikelyAssistantEcho(inputText, lastAssistantOutput)
					log.Printf("Gemini input transcript: %q", inputText)
					if echoTranscript {
						log.Printf("Ignoring assistant echo transcript: %q", inputText)
					}
					// Native capture/VAD evidence is diagnostic only. A quiet or
					// immediate command must not lose its valid input transcript.
					if !echoTranscript && requestGuard.acceptTranscript(inputText) {
						meaningfulInput.Store(true)
						lastMeaningfulAt.Store(time.Now().UnixNano())
						s.touchActivity()
						if !nativeInputActive.Load() && !responding {
							c.setPhase("thinking")
						}
					}
					recognitionText.WriteString(text)
					if !echoTranscript && conversationDone(inputText) {
						rememberReply("[Пользователь завершил разговор.]")
						s.stopPlayer()
						_ = ducking.Restore()
						cancel()
						return
					}
					action := ""
					if !echoTranscript && (meaningfulInput.Load() || requestGuard.allowResponse()) {
						action = quickMediaAction(inputText)
					}
					if !echoTranscript && (meaningfulInput.Load() || requestGuard.allowResponse()) && isStopCommand(inputText) {
						if isMediaPauseCommand(inputText) {
							action = "pause"
						} else {
							// "Замолчи" stops the assistant, not the music.
							rememberReply("[Пользователь попросил замолчать.]")
							s.stopPlayer()
							_ = ducking.Restore()
							cancel()
							return
						}
					}
					if action != "" && commands.allowAction(time.Now(), "media_control") {
						recognitionAction = "media:" + action
						log.Println("Quick voice media action:", action)
						s.stopPlayer()
						// Use the same native media action as the model's tools,
						// including Spotify fallback, for either input language.
						result := tool("media_control", M{"action": action})
						if result["ok"] != true {
							log.Println("Voice media control failed:", result["error"])
						}
						if action == "pause" || action == "stop" {
							mediaStopped = true
							_ = ducking.OnExplicitPause()
						} else if action == "resume" || action == "play" {
							mediaStopped = false
						}
						if result["ok"] == true {
							rememberReply("[Выполнено управление музыкой: " + action + ".]")
						}
						_ = ducking.Restore()
						cancel()
						return
					}
					if cfg.TestTrace {
						c.mu.Lock()
						c.stats["test_input_transcript"] = inputText
						c.mu.Unlock()
					}
					// Do not touch the idle timer for every transcript: noise
					// transcripts arrive here too. Only an accepted request
					// (handled above) is real activity.
					if !voiceSignals {
						commands.input()
					}
					c.mu.Lock()
					c.stats["input_speech_events"] = commands.inputRevision
					c.mu.Unlock()
				}
			}
			if content["interrupted"] == true {
				responding = false
				log.Println("Gemini interrupted")
				s.stopPlayer()
				if !s.inputClosed.Load() {
					c.setPhase("listening")
				}
			}
			if audio := requestGuard.takeAudio(); len(audio) > 0 {
				if e := playReplyAudio(audio); e != nil {
					c.mu.Lock()
					c.lastError = "playback failed: " + clean(e)
					c.mu.Unlock()
					return
				}
			}
			if pendingTurnComplete && requestGuard.allowResponse() {
				content["turnComplete"] = true
				pendingTurnComplete = false
			}
			if turn, ok := content["modelTurn"].(map[string]any); ok {
				if parts, ok := turn["parts"].([]any); ok {
					for _, p := range parts {
						part, _ := p.(map[string]any)
						if inline, ok := part["inlineData"].(map[string]any); ok {
							encoded, _ := inline["data"].(string)
							audio, e := base64.StdEncoding.DecodeString(encoded)
							if e == nil && len(audio) > 0 {
								if isSilentOrEmptyOutput(outputText) && outputText != "" {
									continue
								}
								if allowTurnResponse() {
									requestGuard.markMeaningful()
									if pending := requestGuard.takeAudio(); len(pending) > 0 {
										audio = append(pending, audio...)
									}
								} else {
									audio = requestGuard.queueAudio(audio)
								}
								if len(audio) == 0 {
									continue
								}
								replyAudioSinceInput = true
								if e = playReplyAudio(audio); e != nil {
									c.mu.Lock()
									c.lastError = "playback failed: " + clean(e)
									c.mu.Unlock()
									return
								}
							}
						}
					}
				}
			}
			if content["turnComplete"] == true {
				// A tool boundary is not a completed spoken answer. Keep both
				// the microphone and LEDs in the model-owned state until the
				// final response arrives.
				if (awaitingToolResponse.Load() && musicSelection == nil) || data["toolCall"] != nil || len(pendingToolCalls) > 0 {
					content["turnComplete"] = false
				}
			}
			if content["turnComplete"] == true {
				if isSilentOrEmptyOutput(outputText) && musicSelection != nil {
					// The model prepared a track but produced no announcement
					// audio. Launch it instead of leaving the mic closed, the
					// startup silence held, and then pausing the new track in
					// session cleanup.
					selection := musicSelection
					musicSelection = nil
					c.setPhase("launching_music")
					result := <-musicLaunch
					musicLaunch = nil
					if result["ok"] == true && !musicMatches(selection, result) {
						_, _ = wampCall("com.harman.gva.pause")
						result = M{"ok": false, "status": "wrong_track_rejected", "error": "Spotify запустил другой трек."}
					}
					log.Println("Spotify launch result (silent announcement)", result["status"], result["ok"], result["error"])
					if result["ok"] == true {
						musicStarted = true
						return
					}
					_ = releaseMusicSilence()
					releaseMusicSilence = nil
					c.setPhase("thinking")
					if err := s.send(M{"clientContent": M{"turns": []M{{"role": "user", "parts": []M{{"text": "Запуск Spotify не подтверждён. Коротко сообщи об ошибке, не вызывай функции."}}}}, "turnComplete": true}}); err != nil {
						return
					}
					finalReplyDeadline.Store(time.Now().Add(10 * time.Second).UnixNano())
					s.touchActivity()
					continue
				}
				if isSilentOrEmptyOutput(outputText) {
					nativeAwaitingResult.Store(false)
					pendingTurnComplete = false
					requestGuard.reset()
					s.stopPlayer()
					c.setPhase("listening")
					continue
				}
				if !allowTurnResponse() {
					suppressedOutput := outputText
					// Capture before clearing: a bare "Hey Google" often makes
					// the model answer without any user transcript. That
					// acknowledgement must not disarm the mic for the first
					// command.
					hadUserInput := strings.TrimSpace(inputText) != "" || meaningfulInput.Load()
					initialWake := isInitialWake.Load()
					nativeAwaitingResult.Store(false)
					pendingTurnComplete = false
					requestGuard.reset()
					pendingToolCalls = nil
					// A suppressed turn is noise. Drop every piece of state that
					// could keep the session alive or authorize the next reply,
					// but do not touch the idle timer: the normal idle watchdog
					// must be able to close a noise-only session.
					postWakeSpeech.Store(false)
					meaningfulInput.Store(false)
					replyAudioSinceInput = false
					inputText, outputText = "", ""
					commands.end()
					if initialWake && !hadUserInput {
						// No user request yet: keep listening for the first
						// command instead of closing the wake window.
						armFollowUp(5 * time.Second)
						log.Println("Initial wake acknowledgment; keeping microphone armed for first command")
					} else {
						nativeAllowStart.Store(false)
						followUpUntil.Store(0)
						clearFollowupGate()
					}
					c.mu.Lock()
					count, _ := c.stats["unprompted_suppressed_turns"].(int)
					c.stats["unprompted_suppressed_turns"] = count + 1
					c.mu.Unlock()
					log.Printf("Unprompted turn suppressed (isInitialWake=%v); output was %q", isInitialWake.Load(), suppressedOutput)
					c.setPhase("listening")
					isInitialWake.Store(false)
					continue
				}
				initialWake := isInitialWake.Load()
				isInitialWake.Store(false)
				if !voiceSignals && !nativeInputActive.Load() {
					commands.end()
				}
				responding = false
				log.Printf("Gemini turn complete; output: %q", outputText)
				s.finishPlayer(false)
				rememberReply(outputText)
				lastAssistantOutput = outputText
				if musicSelection != nil && replyAudioSinceInput {
					selection := musicSelection
					musicSelection = nil
					c.setPhase("launching_music")
					announcementDone := monotonicMicros()
					result := <-musicLaunch
					musicLaunch = nil
					if result["ok"] == true && !musicMatches(selection, result) {
						_, _ = wampCall("com.harman.gva.pause")
						result = M{"ok": false, "status": "wrong_track_rejected", "error": "Spotify запустил другой трек."}
					}
					c.mu.Lock()
					c.stats["test_music_request"] = selection
					c.stats["spotify_result"] = result
					c.stats["test_music_launch_us"] = monotonicMicros()
					c.stats["music_wait_after_announcement_ms"] = (monotonicMicros() - announcementDone) / 1000
					c.mu.Unlock()
					log.Println("Spotify launch result", result["status"], result["ok"], result["error"])
					if result["ok"] == true {
						musicStarted = true
						return
					}
					_ = releaseMusicSilence()
					releaseMusicSilence = nil
					replyAudioSinceInput = false
					c.setPhase("thinking")
					if err := s.send(M{"clientContent": M{"turns": []M{{"role": "user", "parts": []M{{"text": "Запуск Spotify не подтверждён. Коротко сообщи об ошибке, не вызывай функции."}}}}, "turnComplete": true}}); err != nil {
						return
					}
					finalReplyDeadline.Store(time.Now().Add(10 * time.Second).UnixNano())
					s.touchActivity()
					continue
				}
				if data["toolCall"] == nil && len(pendingToolCalls) == 0 {
					awaitingToolResponse.Store(false)
				}
				// A tool invocation can emit turnComplete before its tool response
				// produces the final spoken answer. That boundary must not close WS.
				if endAfterTurn && data["toolCall"] == nil && len(pendingToolCalls) == 0 {
					if !shouldHonorEndSession(outputText) {
						log.Printf("Ignoring endAfterTurn: output requested user reply: %q", outputText)
						endAfterTurn = false
					} else {
						return
					}
				}
				if !awaitingToolResponse.Load() && data["toolCall"] == nil && len(pendingToolCalls) == 0 {
					nativeAwaitingResult.Store(false)
					// Assistant finished speaking: resume listening for follow-up speech
					if replyAudioSinceInput {
						completedReply := outputText
						// Capture before resetting: the first command may not
						// have produced a transcript yet when the model answers
						// a bare "Hey Google".
						hadUserInput := strings.TrimSpace(inputText) != "" || meaningfulInput.Load()
						// A completed reply is not permission for another reply.
						// Reopen only after fresh user audio and a new input transcript.
						requestGuard.reset()
						pendingToolCalls = nil
						meaningfulInput.Store(false)
						postWakeSpeech.Store(false)
						nativeInputActive.Store(false)
						inputText, outputText = "", ""
						replyAudioSinceInput = false
						// Keep the mic armed for an explicit question, or for a
						// short one-word follow-up after a plain answer. The
						// short window is bounded and disarmed when it expires,
						// so noise cannot keep the session alive. The initial
						// wake acknowledgement is the exception: it is not a
						// reply to a user request.
						if initialWake && !hadUserInput {
							armFollowUp(5 * time.Second)
							log.Println("Initial wake acknowledgment; keeping microphone armed for first command")
						} else if followUpAllowedByReply(completedReply) {
							if decision := micReopen.decide(time.Now(), sourceMicReopen, false, false, false); decision == micReopenIgnoredRateLimit {
								log.Println("Mic reopen rate limited; closing follow-up window")
								nativeAllowStart.Store(false)
								followUpUntil.Store(0)
								clearFollowupGate()
							} else {
								followUpWindow := 6 * time.Second
								if musicAtWake {
									followUpWindow = 4 * time.Second
								}
								armFollowUp(followUpWindow)
							}
						} else {
							// Even a plain answer gets a bounded window for a
							// one-word follow-up ("ещё", "да", "нет",
							// "спасибо"). Four seconds gives the user time to
							// start a short word, but the window is disarmed
							// when it expires so noise cannot keep the mic open.
							followUpWindow := 6 * time.Second
							if musicAtWake {
								followUpWindow = 4 * time.Second
							}
							armFollowUp(followUpWindow)
							log.Println("Follow-up scheduled after playback tail")
						}
					}
					s.touchActivity()
				}
			}
		}
		if call, ok := data["toolCall"].(map[string]any); ok {
			calls, _ := call["functionCalls"].([]any)
			if len(pendingToolCalls)+len(calls) > 8 {
				return
			}
			pendingToolCalls = append(pendingToolCalls, calls...)
			delete(data, "toolCall")
			// A tool call is model output, not evidence of a user request.
			// Release it only when a transcript/VAD already authorised the turn.
		}
		if allowTurnResponse() && len(pendingToolCalls) > 0 {
			// Once input authorizes the turn, always return a tool response,
			// including validation failures. Waiting for another model event
			// here deadlocks when the model is waiting for this tool result.
			data["toolCall"] = map[string]any{"functionCalls": pendingToolCalls}
			pendingToolCalls = nil
		}
		if call, ok := data["toolCall"].(map[string]any); ok {
			s.touchActivity()
			if calls, ok := call["functionCalls"].([]any); ok {
				awaitingToolResponse.Store(true)
				nativeAllowStart.Store(false)
				followUpUntil.Store(0)
				clearFollowupGate()
				finalReplyDeadline.Store(0)
				// A short progress announcement precedes the lookup, not the
				// final answer. Drain it and leave speaking state while the tool
				// works so the partial-audio watchdog cannot cancel the lookup.
				for _, item := range calls {
					f, _ := item.(map[string]any)
					if f["name"] == "web_search" || f["name"] == "get_news" {
						if responding {
							s.finishPlayer(false)
						}
						responding = false
						replyAudioSinceInput = false
						lastReplyAudioAt.Store(0)
						outputText = ""
						break
					}
				}
				if !responding {
					c.setPhase("thinking")
				}
				responses := []M{}
				completedTools := []completedVoiceTool{}
				for _, item := range calls {
					f, _ := item.(map[string]any)
					name, _ := f["name"].(string)
					if name != "end_session" || recognitionAction == "no_action" {
						recognitionAction = name
						if name == "play_music" {
							details, _ := json.Marshal(f["args"])
							recognitionAction += ":" + boundedConversationText(string(details), 350)
						}
						if name == "spotify_dj_prompt" {
							djArgs, _ := f["args"].(map[string]any)
							prompt, _ := djArgs["prompt"].(string)
							recognitionAction = "spotify_dj_prompt:" + boundedConversationText(prompt, 200)
						}
						if name == "media_control" {
							mediaArgs, _ := f["args"].(map[string]any)
							action, _ := mediaArgs["action"].(string)
							recognitionAction = "media:" + action
						}
					}
					args, _ := f["args"].(map[string]any)
					if name == "adjust_volume" {
						args = spokenVolumeAdjustment(args, inputText)
					}
					id, _ := f["id"].(string)
					toolStarted := time.Now()
					log.Printf("native tool %s started", name)
					s.touchActivity()
					result, cached := toolResults[id]
					if !cached || id == "" {
						if name == "end_session" {
							if replyAudioSinceInput {
								result = M{"ok": true, "finish_after_reply": true, "instruction": "Ответ уже озвучен. Ничего не повторяй и заверши ход."}
							} else {
								result = toolContext(ctx, name, args)
							}
						} else if (name == "get_weather" || name == "get_time") && (conversationDone(inputText) || isStopCommand(inputText)) {
							result = M{"ok": true, "ignored": true, "instruction": "Пользователь попросил остановиться или отказался. Ничего не отвечай и заверши ход."}
						} else if name == "get_weather" && !spokenWeatherIntent(inputText) && (strings.TrimSpace(inputText) != "" || !requestGuard.allowResponse()) {
							result = M{"ok": true, "ignored": true, "instruction": "В запросе нет погоды. Не вызывай get_weather; ответь по существу или переспроси."}
						} else if (name == "set_volume" || name == "adjust_volume" || name == "get_volume") && !spokenVolumeIntent(inputText) && (strings.TrimSpace(inputText) != "" || !requestGuard.allowResponse()) {
							result = M{"ok": true, "ignored": true, "instruction": "В запросе нет команды громкости. Не меняй громкость; ответь по существу или переспроси."}
						} else if name == "broadcast" && !spokenBroadcastIntent(inputText) {
							result = M{"ok": false, "error": "Нет просьбы о трансляции. Произнеси запрошенный текст сам, без инструмента broadcast."}
						} else if commands.allowAction(time.Now(), name) {
							if name == "play_music" {
								if cfg.TestTrace {
									c.mu.Lock()
									c.stats["test_model_music_fields"] = args
									c.mu.Unlock()
								}
								args = literalMusicFields(args, inputText)
								if validation, ok := args["validation_error"].(string); ok {
									result = M{"ok": false, "error": validation}
								}
								trackVal, _ := args["track"].(string)
								artistVal, _ := args["artist"].(string)
								isGenericPlay := (strings.TrimSpace(trackVal) == "" || trackVal == "музыка" || strings.EqualFold(trackVal, "play") || strings.EqualFold(trackVal, "плей")) && strings.TrimSpace(artistVal) == ""
								if result == nil && isGenericPlay {
									if native := readSpotifyNative(); native.Paused || (native.Track.URI != "" && !native.Stopped) {
										actionResult := toolContext(ctx, "media_control", M{"action": "resume"})
										if actionResult["ok"] == true {
											result = M{"ok": true, "status": "ready_to_play", "announcement": "Продолжаю воспроизведение."}
										} else {
											result = actionResult
										}
									}
								}
								if result == nil {
									cueDone := make(chan struct{})
									cue := time.AfterFunc(750*time.Millisecond, func() {
										defer close(cueDone)
										if ctx.Err() != nil || nativeInputActive.Load() {
											return
										}
										if err := s.play(spotifyWaitCue()); err == nil {
											s.finishPlayer(false)
											log.Println("Spotify search wait cue played")
										}
									})
									result = toolContext(ctx, name, args)
									if !cue.Stop() {
										<-cueDone
									}
								}
							} else {
								result = toolContext(ctx, name, args)
							}
						} else {
							result = M{"ok": false, "error": "Команда слишком частая или уже выполняется. Ответь пользователю обычным голосом, не вызывай функции повторно."}
						}
						if id != "" {
							toolResults[id] = result
						}
					}
					completedTools = append(completedTools, completedVoiceTool{name, result["ok"] == true})
					if cfg.TestTrace {
						c.mu.Lock()
						tools, _ := c.stats["test_tools"].([]M)
						tools = append(tools, M{"name": name, "args": args, "ok": result["ok"], "error": result["error"]})
						if len(tools) > 8 {
							tools = tools[len(tools)-8:]
						}
						c.stats["test_tools"] = tools
						c.mu.Unlock()
					}
					if name == "end_session" && result["ok"] == true {
						if shouldHonorEndSession(outputText) {
							endAfterTurn = true
						} else {
							log.Printf("Ignoring end_session: output asks for a repeat/question: %q", outputText)
							result["instruction"] = "Продолжай обычный разговор. Не повторяй уже сказанное."
						}
					} else if (name == "set_volume" || name == "adjust_volume" || name == "media_control") && result["ok"] == true {
						endAfterTurn = true
					}
					if name == "play_music" && result["ok"] == true && (!cached || id == "") {
						release, err := ducking.holdSpotifyStartupSilence()
						if err != nil {
							result = M{"ok": false, "error": "Не удалось подготовить тихий запуск Spotify."}
						} else {
							releaseMusicSilence = release
							musicSelection = result
							musicLaunch = make(chan M, 1)
							go func(selection M, launched chan<- M) {
								launched <- directSpotify.playContext(ctx, selection)
							}(musicSelection, musicLaunch)
							s.finishPlayer(false)
							responding = false
							replyAudioSinceInput = false
							closeInput()
							c.setPhase("announcing_music")
						}
					}
					if name == "media_control" && result["ok"] == true {
						action, _ := args["action"].(string)
						if action == "pause" || action == "stop" {
							mediaStopped = true
							_ = ducking.Restore()
						} else if action == "resume" || action == "play" {
							mediaStopped = false
						}
					}
					response := result
					if name == "web_search" {
						response = compactSearchToolResponse(result)
					}
					responses = append(responses, M{"id": f["id"], "name": name, "response": response})
					log.Println("native tool", name, "ok", result["ok"], "status", result["status"], "error", result["error"])
					log.Printf("native tool %s duration=%s cached=%v", name, time.Since(toolStarted), cached)
				}
				if controlsComplete(completedTools) {
					rememberReply("[Команда управления выполнена.]")
					s.stopPlayer()
					closeInput()
					for _, tool := range completedTools {
						if tool.name == "set_volume" || tool.name == "adjust_volume" {
							// Cast schedules the native volume sound asynchronously.
							// Keep MEDIA ducked while its effect stream drains.
							_ = waitSpotifyRegistration(ctx, 200*time.Millisecond)
							break
						}
					}
					return
				}
				if endAfterTurn && replyAudioSinceInput {
					s.finishPlayer(false)
					closeInput()
					return
				}
				if endAfterTurn {
					closeInput()
					c.setPhase("thinking")
				}
				if err := s.send(M{"toolResponse": M{"functionResponses": responses}}); err != nil {
					c.mu.Lock()
					c.lastError = "tool response upload failed: " + clean(err)
					c.mu.Unlock()
					return
				}
				// Some turns finish without delivering audio after a tool call.
				// Only bound the wait after sending the result, never capture.
				deadline := 10 * time.Second
				if endAfterTurn {
					deadline = 3 * time.Second
				}
				finalReplyDeadline.Store(time.Now().Add(deadline).UnixNano())
				s.touchActivity()
			}
		}
	}
}
func watch() {
	for {
		func() {
			c, err := wampConn()
			if err != nil {
				return
			}
			defer c.Close()
			lastVoice := ""
			lastWakeTime := time.Time{}
			c.SetReadDeadline(time.Time{})
			topics := []string{"com.harman.stateChanged", "com.harman.test.inputEvent"}
			for i, t := range topics {
				c.WriteJSON([]any{32, i + 1, M{}, t})
			}
			if cfg.NativeBehavior && !ctl.busy() {
				go func() { _ = applyNativePhase("idle") }()
			}
			subscriptions := map[float64]string{}
			for {
				var event []any
				if c.ReadJSON(&event) != nil {
					return
				}
				if len(event) >= 3 && event[0] == float64(33) {
					request, ok := event[1].(float64)
					sub, subOK := event[2].(float64)
					if ok && subOK && request >= 1 && request <= float64(len(topics)) {
						subscriptions[sub] = topics[int(request)-1]
					}
					continue
				}
				if len(event) < 5 || event[0] != float64(36) {
					continue
				}
				subscription, _ := event[1].(float64)
				topic := subscriptions[subscription]
				if cfg.NativeBehavior && topic == "com.harman.test.inputEvent" {
					switch nativeInputButton(event) {
					case "action", "action-long":
						// Volume/micmute stay with the native audio-ui handler to
						// avoid double steps/toggles; action is ours because it
						// controls the Gemini session.
						if handleNativeInputEvent(event) {
							continue
						}
					}
				}
				if topic != "com.harman.stateChanged" {
					continue
				}
				if len(event) < 6 {
					continue
				}
				state, ok := event[5].(map[string]any)
				if !ok {
					continue
				}
				if microphone, ok := state["microphone"].(map[string]any); ok {
					muted := microphone["state"] == "mic-muted"
					if muted {
						if !assistantBackgroundActive() {
							ctl.stop()
						}
						continue
					}
				}
				voice, _ := state["voice"].(map[string]any)
				voiceState, _ := voice["state"].(string)
				// Our AudioUI mirroring is not a fresh hardware wake word.
				if voice["citation-origin"] == "gemini" {
					continue
				}
				if (voiceState == "assistant-cancelled" || voiceState == "assistant-idle") && cfg.NativeBehavior && nativeHandoff.Load() == 0 && !assistantBackgroundActive() {
					if phase := ctl.currentPhase(); phase != "" && phase != "idle" {
						if phase == "connecting" || phase == "opening_audio" {
							phase = "listening"
						}
						go reassertNativePhase(phase)
					} else if voice["btn-action-long"] != "noop" {
						go func() { _ = applyNativePhase("idle") }()
					}
					lastVoice = voiceState
					continue
				}
				if voiceState == "assistant-listening" && cfg.AutoWake && nativeHandoff.Load() == 0 {
					if assistantBackgroundActive() {
						// This is our own background Assistant action, not a new
						// user wake. Do not start or interrupt Gemini.
						lastVoice = voiceState
						continue
					}
					// Every real wake belongs to Gemini, including repeats while
					// connecting. Debouncing must not leave Google's interaction alive.
					nativeStopped := make(chan struct{})
					go func() {
						defer close(nativeStopped)
						_ = grpcCall("StopInteraction", []byte{8, 1})
						_ = grpcCall("StopAllTts", nil)
					}()
					if wakeSuppressed() {
						// Native hotword detector heard our own TTS/cue. The
						// StopInteraction above already cancelled its Google
						// interaction; do not interrupt or restart Gemini.
						lastVoice = voiceState
						continue
					}
					if time.Now().UnixNano() < cancelledWakeUntil.Load() {
						lastVoice = voiceState
						continue
					}
					// If already connecting or listening, ignore echo/duplicate
					if ctl.isListeningOrConnecting() {
						if phase := ctl.currentPhase(); phase != "" && phase != "idle" {
							if phase == "connecting" || phase == "opening_audio" {
								phase = "listening"
							}
							go reassertNativePhase(phase)
						}
						lastVoice = voiceState
						continue
					}
					// Debounce: ignore repeated events for the same wake detection
					if time.Since(lastWakeTime) < 1500*time.Millisecond {
						lastVoice = voiceState
						continue
					}
					// Genuine new wake event: only trigger if state transitioned or previous wake was long ago
					if lastVoice != voiceState || time.Since(lastWakeTime) > 2000*time.Millisecond {
						lastWakeTime = time.Now()
						lastVoice = voiceState
						if ctl.busy() {
							log.Println("Wake word received while busy; interrupting prior session")
							ctl.stop()
						}
						// Capture must subscribe at the wake event, before RPC timeouts.
						go ctl.startAfterNativeWake(nativeStopped)
					}
					continue
				}
				lastVoice = voiceState
			}
		}()
		time.Sleep(3 * time.Second)
	}
}
func main() {
	runtime.GOMAXPROCS(2)
	log.SetFlags(log.LstdFlags)
	mode := "info"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if mode == "info" {
		fmt.Println(`{"native":true,"architecture":"` + runtime.GOARCH + `","glibc_required":false}`)
		return
	}
	if mode == "clean-ipc" {
		if err := cleanDsnoopIPC(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if mode == "spotify-pcm" {
		if len(os.Args) != 3 {
			log.Fatal("spotify-pcm requires a FIFO path")
		}
		if err := serveSpotifyPCM(os.Args[2]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if mode == "spotify-home" {
		log.Fatal(serveSpotifyHome())
	}
	if mode == "spotify-cast-stop" {
		if e := stopSpotifyCastApp(); e != nil {
			log.Fatal(e)
		}
		fmt.Println(`{"spotify_receiver_stopped":true}`)
		return
	}
	if mode == "native-ui-idle" {
		_, e := wampCallTimeout(time.Second, "com.harman.extStateUpdate", M{"state": "assistant-idle", "animation": "", "transition": ""}, "voice")
		if e != nil {
			log.Fatal(clean(e))
		}
		_, _ = wampCall("com.harman.ledAnimate", "animation_close")
		return
	}
	if mode == "spotify-info" || mode == "spotify-resolve" || mode == "spotify-wake" || mode == "spotify-play" {
		directSpotify.wakeReceiver = directSpotify.wakeLocalSpotifyReceiver
		var result M
		if mode == "spotify-info" {
			devices, e := directSpotify.devices()
			result = M{"connected": directSpotify.connected(), "devices": devices, "error": clean(e)}
		} else if mode == "spotify-wake" {
			e := directSpotify.wakeLocalSpotifyReceiver()
			devices, _ := directSpotify.devices()
			result = M{"wake_error": clean(e), "devices": devices}
		} else if mode == "spotify-play" {
			if len(os.Args) < 4 {
				log.Fatal("music type and name required")
			}
			resolved := directSpotify.resolve(prepareMusic(M{"music_type": os.Args[2], "track": os.Args[3]}))
			if resolved["ok"] != true {
				result = resolved
			} else {
				result = directSpotify.play(resolved)
			}
		} else {
			if len(os.Args) < 4 {
				log.Fatal("music type and name required")
			}
			result = directSpotify.resolve(prepareMusic(M{"music_type": os.Args[2], "track": os.Args[3]}))
		}
		b, _ := json.Marshal(result)
		fmt.Println(string(b))
		return
	}
	if mode == "mixer-check" || mode == "mixer-play" || mode == "echo-probe" || mode == "native-query" || mode == "native-spotify" || mode == "music-status" || mode == "music-confirm" {
		if os.Geteuid() == 0 {
			if err := syscall.Setgroups([]int{100, 1000, 1005, 1006, 1008, 1009, 1010, 1012, 1014, 1015, 1016, 3003, 3008}); err != nil {
				log.Fatal(err)
			}
			if err := syscall.Setgid(3003); err != nil {
				log.Fatal(err)
			}
			if err := syscall.Setuid(1000); err != nil {
				log.Fatal(err)
			}
		}
		var e error
		if mode == "music-confirm" {
			if len(os.Args) < 3 {
				log.Fatal("song query required")
			}
			result := confirmedSpotify(os.Args[2])
			b, _ := json.Marshal(result)
			fmt.Println(string(b))
		} else if mode == "music-status" {
			var status playbackStatus
			status, e = readPlaybackStatus()
			if e == nil {
				b, _ := json.Marshal(status)
				fmt.Println(string(b))
			}
		} else if mode == "native-spotify" {
			if len(os.Args) < 3 {
				log.Fatal("song query required")
			}
			var quiet bool
			quiet, e = nativeSpotify(os.Args[2])
			if e == nil {
				fmt.Printf("{\"query_accepted\":true,\"native_confirmation_stop_sent\":%t}\n", quiet)
			}
		} else if mode == "native-query" {
			if len(os.Args) < 3 {
				log.Fatal("text query required")
			}
			e = grpcCall("SendTextQuery", pbMessage(1, []byte(os.Args[2])))
			if e == nil {
				fmt.Println(`{"query_accepted":true,"playback_verified":false}`)
			}
		} else if mode == "echo-probe" {
			if len(os.Args) < 3 {
				log.Fatal("WAV path required")
			}
			e = echoProbe(os.Args[2])
		} else if mode == "mixer-play" {
			if len(os.Args) < 3 {
				log.Fatal("WAV path required")
			}
			e = mixerPlayFile(os.Args[2])
		} else {
			e = mixerCheck()
		}
		if e != nil {
			log.Fatal(e)
		}
		return
	}
	path := "/data/opt/citation-gemini/config.json"
	if len(os.Args) > 2 && mode != "search-probe" && mode != "live-search-probe" && mode != "web-probe" && mode != "ddg-probe" && mode != "currency-probe" {
		path = os.Args[2]
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatal("configuration file unavailable")
	}
	if json.Unmarshal(b, &cfg) != nil {
		log.Fatal("invalid configuration")
	}
	if cfg.Model == "" {
		cfg.Model = "models/gemini-3.8-live"
	}
	b, err = os.ReadFile(cfg.KeyFile)
	if err != nil {
		log.Fatal("API key file unavailable")
	}
	key = strings.TrimSpace(string(b))
	if key == "" {
		log.Fatal("API key is empty")
	}
	if mode == "search-probe" {
		if len(os.Args) < 3 {
			log.Fatal("search query required")
		}
		res, err := geminiGroundedSearch(context.Background(), os.Args[2])
		if err != nil {
			fmt.Println(M{"ok": false, "error": clean(err)})
			return
		}
		fmt.Println(res)
		return
	}
	if mode == "live-search-probe" {
		if len(os.Args) < 3 {
			log.Fatal("search query required")
		}
		res, err := geminiLiveSearch(context.Background(), os.Args[2])
		if err != nil {
			fmt.Println(M{"ok": false, "error": clean(err)})
			return
		}
		fmt.Println(res)
		return
	}
	if mode == "web-probe" {
		if len(os.Args) < 3 {
			log.Fatal("search query required")
		}
		res, _ := handleSearchTool(context.Background(), "web_search", M{"query": os.Args[2]})
		fmt.Println(res)
		return
	}
	if mode == "ddg-probe" {
		if len(os.Args) < 3 {
			log.Fatal("search query required")
		}
		res, err := duckDuckGoSearch(context.Background(), os.Args[2])
		if err != nil {
			fmt.Println(M{"ok": false, "error": clean(err)})
			return
		}
		fmt.Println(res)
		return
	}
	if mode == "currency-probe" {
		if len(os.Args) < 4 {
			log.Fatal("currency probe requires FROM and TO")
		}
		res, _ := handleSearchTool(context.Background(), "convert_currency", M{"value": 1.0, "from": os.Args[2], "to": os.Args[3]})
		fmt.Println(res)
		return
	}
	if mode == "models-probe" {
		req, err := http.NewRequest("GET", "https://generativelanguage.googleapis.com/v1beta/models", nil)
		if err != nil {
			fmt.Println(M{"ok": false, "error": clean(err)})
			return
		}
		req.Header.Set("x-goog-api-key", key)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Println(M{"ok": false, "error": clean(err)})
			return
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		var out struct {
			Models []struct {
				Name                       string   `json:"name"`
				SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			fmt.Println(M{"ok": false, "error": clean(err)})
			return
		}
		for _, m := range out.Models {
			for _, method := range m.SupportedGenerationMethods {
				if method == "generateContent" {
					fmt.Println(m.Name)
					break
				}
			}
		}
		return
	}
	if mode == "check" {
		c, e := cloud()
		if e != nil {
			log.Fatal(clean(e))
		}
		c.Close()
		fmt.Println(`{"live_setup":true,"native":true}`)
		return
	}
	if mode != "serve" {
		log.Fatal("use info, check, or serve")
	}
	if os.Geteuid() == 0 {
		_ = filepath.Walk(stateDirectory, func(p string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chown(p, 1000, 3003)
			}
			return nil
		})
		if err := syscall.Setgroups([]int{100, 1000, 1005, 1006, 1008, 1009, 1010, 1012, 1014, 1015, 1016, 3003, 3008}); err != nil {
			log.Fatal(err)
		}
		if err := syscall.Setgid(3003); err != nil {
			log.Fatal(err)
		}
		if err := syscall.Setuid(1000); err != nil {
			log.Fatal(err)
		}
	}
	if cfg.Playback == "" || cfg.AlsaConfig == "" {
		log.Fatal("native audio configuration required")
	}
	loadPreferences()
	// Speaker calibration is deliberately not loaded into the capture path.
	timerAnnounce = func(text string) {
		if ctl.hasActiveSession() {
			go ctl.announceToActiveSession(text)
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			if err := speakGeminiTTS(ctx, text); err != nil {
				log.Println("Gemini reminder TTS:", clean(err))
			}
		}()
	}
	initTimerManager()
	if source, err := NewGoogleTokenSourceFromEnv(); err == nil {
		if st := source.Status(); st.Authorized {
			setReminderBackend(NewCalendarSync(stateDirectory, source))
			log.Printf("Google Calendar/Tasks reminder sync enabled for %s", st.Email)
		} else {
			log.Printf("Google Calendar/Tasks sync not authorized; using local reminders")
		}
	} else {
		log.Printf("Google Calendar/Tasks sync unavailable: %v", clean(err))
	}
	daemonLock, lockError := os.OpenFile(stateDirectory+"/daemon.lock", os.O_CREATE|os.O_RDWR, 0600)
	if lockError != nil {
		log.Fatal("daemon lock unavailable")
	}
	defer daemonLock.Close()
	if syscall.Flock(int(daemonLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		log.Fatal("native Gemini is already running")
	}
	_ = ducking.Reset()
	go ducking.refreshUntilShutdown()
	registerSpotifyHandlers()
	registerEqualizerHandlers()
	registerUplinkRecordingHandlers()
	if !usingNativeCaptureService() {
		go keepAECReference()
	}
	if err := startPersistentCapture(); err != nil {
		log.Println("persistent capture start warning:", clean(err))
	}
	if cfg.ReplaceGoogle {
		go guardGoogleVoice()
	}
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		ctl.mu.Lock()
		defer ctl.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(M{"state": ctl.state, "error": ctl.lastError, "native": true, "auto_wake": cfg.AutoWake, "replace_google": cfg.ReplaceGoogle, "google_voice_guard_active": googleVoiceGuardActive.Load(), "native_behavior": cfg.NativeBehavior, "model": cfg.Model, "audio": "native Cast mixer + firmware AEC", "microphone_backend": cfg.MicrophoneBackend, "capture_health": captureHealth(), "stats": ctl.stats, "recent_voice_inputs": recentRecognitions(), "voice_match": voiceMatchStatus(), "background_assistant": assistantBackgroundActive(), "assistant_tts_samples": assistantTTSAudioSamples.Load(), "assistant_tts_meta_events": assistantTTSMetaEvents.Load(), "google_oauth": currentGoogleOAuthStatus()})
	})
	http.HandleFunc("/google/news-cookies", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var body struct {
			Cookies map[string]string `json:"cookies"`
			Cookie  string            `json:"cookie"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(M{"ok": false, "error": "invalid JSON"})
			return
		}
		cookies := body.Cookies
		if len(cookies) == 0 && body.Cookie != "" {
			cookies = parseCookieHeader(body.Cookie)
		}
		count, err := googleNewsCookies.Save(cookies)
		w.Header().Set("content-type", "application/json")
		if err != nil {
			json.NewEncoder(w).Encode(M{"ok": false, "error": clean(err)})
			return
		}
		json.NewEncoder(w).Encode(M{"ok": true, "count": count, "names": googleNewsCookies.Names()})
	})
	http.HandleFunc("/google/news-cookies/status", func(w http.ResponseWriter, r *http.Request) {
		names := googleNewsCookies.Names()
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(M{"configured": len(names) > 0, "names": names})
	})
	http.HandleFunc("/google/news-cookies/check", func(w http.ResponseWriter, r *http.Request) {
		names := googleNewsCookies.Names()
		valid := false
		if len(names) > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			valid = googleNewsCookiesValid(ctx, googleNewsCookies.CookieHeader())
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(M{"configured": len(names) > 0, "valid": valid, "names": names})
	})
	http.HandleFunc("/google/news-cookies/clear", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(M{"ok": clean(googleNewsCookies.Clear()) == ""})
	})
	http.HandleFunc("/news/google-home", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		useCookies := r.URL.Query().Get("cookies") == "1"
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		res, err := getGoogleNewsHome(ctx, limit, useCookies)
		if err != nil {
			res = M{"ok": false, "error": clean(err)}
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	http.HandleFunc("/news/probe", func(w http.ResponseWriter, r *http.Request) {
		topic := r.URL.Query().Get("topic")
		source := r.URL.Query().Get("source")
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		var res M
		var err error
		if r.URL.Query().Get("personal") == "1" {
			res, err = getPersonalNews(ctx, limit)
		} else {
			res, err = getNews(ctx, topic, source, limit)
		}
		if err != nil {
			res = M{"ok": false, "error": clean(err)}
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	http.HandleFunc("/memory/probe", func(w http.ResponseWriter, r *http.Request) {
		if !requirePersonalAccess(w) {
			return
		}
		action := r.URL.Query().Get("action")
		var res M
		switch action {
		case "add":
			entry, err := personalMemory.Add(r.URL.Query().Get("text"), r.URL.Query().Get("category"))
			if err != nil {
				res = M{"ok": false, "error": clean(err)}
			} else {
				res = M{"ok": true, "entry": entry}
			}
		case "delete":
			n, err := personalMemory.Delete(r.URL.Query().Get("id"), r.URL.Query().Get("text"))
			if err != nil {
				res = M{"ok": false, "error": clean(err)}
			} else {
				res = M{"ok": true, "deleted": n}
			}
		default:
			res = M{"ok": true, "memories": memoryEntriesJSON(personalMemory.List())}
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	http.HandleFunc("/memory/context", func(w http.ResponseWriter, r *http.Request) {
		res, _ := handleMemoryTool("get_personal_context", M{"query": r.URL.Query().Get("query")})
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	http.HandleFunc("/reminders/probe", func(w http.ResponseWriter, r *http.Request) {
		if !requirePersonalAccess(w) {
			return
		}
		text := r.URL.Query().Get("text")
		at := r.URL.Query().Get("at")
		kind := r.URL.Query().Get("kind")
		if kind == "" {
			kind = RemoteKindTask
		}
		res := timerManager.toolSetReminder(M{"text": text, "at": at, "remote_kind": kind})
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	http.HandleFunc("/reminders/list", func(w http.ResponseWriter, r *http.Request) {
		if !requirePersonalAccess(w) {
			return
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(timerManager.toolListReminders())
	})
	http.HandleFunc("/reminders/cancel", func(w http.ResponseWriter, r *http.Request) {
		if !requirePersonalAccess(w) {
			return
		}
		res := timerManager.toolCancelReminder(M{"id": r.URL.Query().Get("id"), "all": r.URL.Query().Get("all") == "1"})
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	http.HandleFunc("/tts/probe", func(w http.ResponseWriter, r *http.Request) {
		text := r.URL.Query().Get("text")
		if strings.TrimSpace(text) == "" {
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(M{"ok": false, "error": "text is required"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		err := speakGeminiTTS(ctx, text)
		w.Header().Set("content-type", "application/json")
		if err != nil {
			json.NewEncoder(w).Encode(M{"ok": false, "error": clean(err)})
			return
		}
		json.NewEncoder(w).Encode(M{"ok": true})
	})
	http.HandleFunc("/voice/arm", func(w http.ResponseWriter, r *http.Request) {
		err := startStockVoiceInteraction()
		time.Sleep(700 * time.Millisecond)
		state, stateErr := wampCallTimeout(2*time.Second, "com.harman.stateGet", M{})
		res := M{"ok": err == nil, "error": clean(err), "background_assistant": assistantBackgroundActive()}
		if stateErr != nil {
			res["state_error"] = clean(stateErr)
		} else {
			res["state"] = state
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	http.HandleFunc("/voice/trigger", func(w http.ResponseWriter, r *http.Request) {
		mode := r.URL.Query().Get("mode")
		var err error
		switch mode {
		case "quicktap":
			_, err = wampCall("com.harman.gva.quickTap")
		case "preset":
			op, _ := strconv.Atoi(r.URL.Query().Get("op"))
			name := r.URL.Query().Get("name")
			if name == "" {
				name = "voice"
			}
			preset := append(pbInt(1, uint64(op)), pbMessage(2, []byte(name))...)
			payload := pbMessage(1, pbMessage(4, preset))
			err = grpcCall("StartInteraction", payload)
		case "enable":
			err = grpcCall("/assistant.api.AudioUtilsService/EnableListening", pbInt(1, 1))
		default:
			err = fmt.Errorf("mode must be quicktap, preset or enable")
		}
		time.Sleep(900 * time.Millisecond)
		state, stateErr := wampCallTimeout(2*time.Second, "com.harman.stateGet", M{})
		res := M{"ok": err == nil, "mode": mode, "trigger_error": clean(err)}
		if stateErr != nil {
			res["state_error"] = clean(stateErr)
		} else {
			res["state"] = state
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	http.HandleFunc("/voice/listen", func(w http.ResponseWriter, r *http.Request) {
		err := grpcCall("StartInteraction", startInteractionPayload())
		time.Sleep(700 * time.Millisecond)
		state, stateErr := wampCallTimeout(2*time.Second, "com.harman.stateGet", M{})
		res := M{"ok": err == nil, "start_error": clean(err)}
		if stateErr != nil {
			res["state_error"] = clean(stateErr)
		} else {
			res["state"] = state
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	http.HandleFunc("/voice/buffer-probe", func(w http.ResponseWriter, r *http.Request) {
		text := r.URL.Query().Get("text")
		if strings.TrimSpace(text) == "" {
			text = "what is my name?"
		}
		pcm := takeVoiceQueryPCM()
		if len(pcm) < 3200 {
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(M{"ok": false, "error": "voice buffer is empty; say a command first"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		res := assistantVoiceQuery(ctx, pcm, text)
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	http.HandleFunc("/voice/probe", func(w http.ResponseWriter, r *http.Request) {
		text := r.URL.Query().Get("text")
		if strings.TrimSpace(text) == "" {
			text = "find my phone"
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		pcm24, err := synthesizeGeminiTTS(ctx, text)
		if err != nil {
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(M{"ok": false, "error": clean(err)})
			return
		}
		pcm16 := resample24kTo16k(pcm24)
		res := assistantVoiceQuery(ctx, pcm16, "")
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	http.HandleFunc("/logs", func(w http.ResponseWriter, r *http.Request) {
		count := 100
		if v, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil && v > 0 {
			count = v
		}
		if count > 500 {
			count = 500
		}
		data, err := os.ReadFile(filepath.Join(stateDirectory, "service.log"))
		if err != nil {
			w.WriteHeader(404)
			fmt.Fprintf(w, "log unavailable: %v", err)
			return
		}
		text := strings.ReplaceAll(string(data), "\r\n", "\n")
		lines := strings.Split(text, "\n")
		if len(lines) > count {
			lines = lines[len(lines)-count:]
		}
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(strings.Join(lines, "\n")))
	})
	http.HandleFunc("/oauth/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(currentGoogleOAuthStatus())
	})
	http.HandleFunc("/oauth/import", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var body struct {
			ClientID     string   `json:"client_id"`
			ClientSecret string   `json:"client_secret"`
			AccessToken  string   `json:"access_token"`
			RefreshToken string   `json:"refresh_token"`
			Expiry       string   `json:"expiry"`
			Scopes       []string `json:"scopes"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(M{"ok": false, "error": "invalid JSON"})
			return
		}
		body.ClientID = strings.TrimSpace(body.ClientID)
		body.ClientSecret = strings.TrimSpace(body.ClientSecret)
		body.RefreshToken = strings.TrimSpace(body.RefreshToken)
		body.AccessToken = strings.TrimSpace(body.AccessToken)
		if body.ClientID == "" || body.RefreshToken == "" {
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(M{"ok": false, "error": "client_id and refresh_token are required"})
			return
		}
		clientPath := filepath.Join(stateDirectory, googleClientFileName)
		clientData, _ := json.MarshalIndent(M{"client_id": body.ClientID, "client_secret": body.ClientSecret}, "", "  ")
		if err := writeAtomicFile(clientPath, clientData, 0600); err != nil {
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(M{"ok": false, "error": clean(err)})
			return
		}
		cfg := GoogleOAuthConfig{ClientID: body.ClientID, ClientSecret: body.ClientSecret, StateDir: stateDirectory, Scopes: GoogleSyncScopes()}
		source, err := NewGoogleTokenSource(cfg)
		if err != nil {
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(M{"ok": false, "error": clean(err)})
			return
		}
		expiry := time.Now().Add(time.Hour)
		if body.Expiry != "" {
			if t, err := time.Parse(time.RFC3339, body.Expiry); err == nil {
				expiry = t
			}
		}
		tok := &GoogleToken{
			AccessToken: body.AccessToken, RefreshToken: body.RefreshToken,
			TokenType: "Bearer", Expiry: expiry, Scopes: body.Scopes,
		}
		if err := source.SaveToken(tok); err != nil {
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(M{"ok": false, "error": clean(err)})
			return
		}
		installGoogleSyncBackend(stateDirectory, source)
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(M{"ok": true, "authorized": true})
	})
	http.HandleFunc("/oauth/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		err := os.Remove(filepath.Join(stateDirectory, googleTokenFileName))
		if err != nil && !os.IsNotExist(err) {
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(M{"ok": false, "error": clean(err)})
			return
		}
		googleSync.Lock()
		googleSync.source = nil
		googleSync.sync = nil
		googleSync.Unlock()
		setReminderBackend(newJSONFileReminderBackend(filepath.Join(stateDirectory, "reminders.json")))
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(M{"ok": true})
	})
	http.HandleFunc("/oauth/client", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var body struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(M{"ok": false, "error": "invalid JSON"})
			return
		}
		body.ClientID = strings.TrimSpace(body.ClientID)
		body.ClientSecret = strings.TrimSpace(body.ClientSecret)
		if body.ClientID == "" {
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(M{"ok": false, "error": "client_id is required"})
			return
		}
		path := filepath.Join(stateDirectory, googleClientFileName)
		data, _ := json.MarshalIndent(body, "", "  ")
		if err := writeAtomicFile(path, data, 0600); err != nil {
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(M{"ok": false, "error": clean(err)})
			return
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(M{"ok": true, "path": path})
	})
	http.HandleFunc("/oauth/start", func(w http.ResponseWriter, r *http.Request) {
		authURL, err := startGooglePKCE()
		w.Header().Set("content-type", "application/json")
		if err != nil {
			json.NewEncoder(w).Encode(M{"ok": false, "error": clean(err)})
			return
		}
		json.NewEncoder(w).Encode(M{"ok": true, "auth_url": authURL, "redirect_uri": googlePKCERedirectURI()})
	})
	http.HandleFunc("/oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		if e := r.URL.Query().Get("error"); e != "" {
			w.WriteHeader(400)
			fmt.Fprintf(w, "Google authorization failed: %s\n", e)
			return
		}
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		tok, err := finishGooglePKCE(ctx, code, state)
		w.Header().Set("content-type", "text/html; charset=utf-8")
		if err != nil {
			w.WriteHeader(400)
			fmt.Fprintf(w, "<h1>Google authorization failed</h1><p>%s</p>", clean(err))
			return
		}
		fmt.Fprintf(w, "<h1>Google authorization complete</h1><p>Account: %s</p><p>You can close this page.</p>", tok.Email)
	})
	http.HandleFunc("/search/probe", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if strings.TrimSpace(q) == "" {
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(M{"ok": false, "error": "q is required"})
			return
		}
		res, _ := handleSearchTool(r.Context(), "web_search", M{"query": q})
		res["normalized"] = normalizeSearchQuery(q)
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	http.HandleFunc("/assistant/query", func(w http.ResponseWriter, r *http.Request) {
		// Local diagnostic endpoint: sends an English text query to the stock
		// Assistant with its TTS muted and returns the response text that the
		// gRPC stream exposes. It is used to validate ecosystem actions.
		if r.Method != "GET" && r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		query := r.URL.Query().Get("q")
		if strings.TrimSpace(query) == "" {
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(M{"ok": false, "error": "q is required"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 14*time.Second)
		defer cancel()
		res := assistantBackgroundQuery(ctx, query)
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	http.HandleFunc("/voice/query.wav", func(w http.ResponseWriter, r *http.Request) {
		wav := voiceQueryWAV()
		if len(wav) == 0 {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("content-type", "audio/wav")
		_, _ = w.Write(wav)
	})
	http.HandleFunc("/assistant/tts.wav", func(w http.ResponseWriter, r *http.Request) {
		wav := assistantTTSWav()
		if len(wav) == 0 {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("content-type", "audio/wav")
		w.Header().Set("content-length", fmt.Sprintf("%d", len(wav)))
		_, _ = w.Write(wav)
	})
	http.HandleFunc("/assistant/textfields", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		field, _ := strconv.Atoi(r.URL.Query().Get("field"))
		value := r.URL.Query().Get("value")
		if strings.TrimSpace(q) == "" || field <= 0 {
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(M{"ok": false, "error": "q and field are required"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
		defer cancel()
		res := assistantBackgroundQueryExtra(ctx, q, pbMessage(field, []byte(value)))
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	http.HandleFunc("/assistant/reflection", func(w http.ResponseWriter, r *http.Request) {
		socket := "/data/chrome/chirp/libassist_grpc.socket"
		switch r.URL.Query().Get("socket") {
		case "audio_input":
			socket = "/data/chrome/chirp/libassist_audio_input.socket"
		case "audio_output":
			socket = "/data/chrome/chirp/libassist_audio_output.socket"
		}
		method := "/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo"
		if r.URL.Query().Get("v1") == "1" {
			method = "/grpc.reflection.v1.ServerReflection/ServerReflectionInfo"
		}
		body, err := grpcCallResponseOnSocket(socket, method, pbMessage(7, nil), 5*time.Second)
		res := M{"ok": err == nil, "socket": socket}
		if err != nil {
			res["error"] = clean(err)
		} else {
			frames := splitGRPCFrames(body)
			res["frames"] = len(frames)
			res["strings"] = extractPrintableStrings(frames)
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	http.HandleFunc("/assistant/raw", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		method := r.URL.Query().Get("method")
		query := r.URL.Query().Get("q")
		ctx, cancel := context.WithTimeout(r.Context(), 14*time.Second)
		defer cancel()
		res := assistantRawQuery(ctx, method, query)
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	http.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		go ctl.start()
		w.WriteHeader(202)
	})
	http.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		ctl.stop()
		w.WriteHeader(202)
	})
	http.HandleFunc("/calibrate", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusGone)
		_ = json.NewEncoder(w).Encode(M{"ok": false, "error": "Speaker calibration is disabled; capture uses the firmware processed stream."})
	})
	http.HandleFunc("/voice-profile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(M{"calibrated": false, "enabled": false, "backend": cfg.MicrophoneBackend})
	})
	http.HandleFunc("/personal-passphrase", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.Method == "GET" {
			_ = json.NewEncoder(w).Encode(M{"configured": personalPassphraseConfigured()})
			return
		}
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var body struct {
			Passphrase string `json:"passphrase"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
			_ = json.NewEncoder(w).Encode(M{"ok": false, "error": "invalid JSON"})
			return
		}
		if err := SetPersonalPassphrase(body.Passphrase); err != nil {
			_ = json.NewEncoder(w).Encode(M{"ok": false, "error": clean(err)})
			return
		}
		_ = json.NewEncoder(w).Encode(M{"ok": true, "configured": true})
	})
	http.HandleFunc("/personal-passphrase/verify", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var body struct {
			Passphrase string `json:"passphrase"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
			_ = json.NewEncoder(w).Encode(M{"ok": false, "error": "invalid JSON"})
			return
		}
		if !VerifyPersonalPassphrase(body.Passphrase) {
			_ = json.NewEncoder(w).Encode(M{"ok": false, "error": "passphrase not recognized"})
			return
		}
		unlockPersonalAccess()
		_ = json.NewEncoder(w).Encode(M{"ok": true, "unlocked_for_seconds": int(personalUnlockWindow.Seconds())})
	})
	if cfg.TestTrace {
		http.HandleFunc("/test/music", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				w.WriteHeader(405)
				return
			}
			ctl.mu.Lock()
			active := ctl.active != nil
			ctl.mu.Unlock()
			if active {
				http.Error(w, "conversation active", 409)
				return
			}
			var args M
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&args) != nil {
				w.WriteHeader(400)
				return
			}
			request := prepareMusic(args)
			if override, _ := args["native_query"].(string); override != "" && len(override) < 600 {
				request["query"] = override
			}
			if args["allow_native_speech"] == true {
				request["allow_native_speech"] = true
			}
			request["test_timeout_seconds"] = args["test_timeout_seconds"]
			request["test_native_locale"] = args["test_native_locale"]
			result := request
			if request["ok"] == true {
				result = confirmedSpotifyRequest(request)
			}
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(result)
		})
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-signals
		stopPersistentCapture()
		ctl.stop()
		for i := 0; i < 100; i++ {
			ctl.mu.Lock()
			idle := ctl.active == nil
			ctl.mu.Unlock()
			if idle {
				os.Exit(0)
			}
			time.Sleep(100 * time.Millisecond)
		}
		os.Exit(1)
	}()
	if cfg.AutoWake {
		go watch()
		if cfg.ReplaceGoogle {
			go watchEarlyHotword()
		}
		if cfg.DJWake {
			go watchDJWake()
		}
	}
	log.Println("Native Gemini control at 127.0.0.1:8766; idle, microphone closed")
	log.Fatal(http.ListenAndServe("127.0.0.1:8766", nil))
}
