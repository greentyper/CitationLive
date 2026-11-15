package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

const stateDirectory = "/data/opt/citation-gemini/state"

var preferenceState = struct {
	sync.Mutex
	City     string `json:"city"`
	Timezone string `json:"timezone"`
}{Timezone: "Asia/Novosibirsk"}

func loadPreferences() {
	b, e := os.ReadFile(filepath.Join(stateDirectory, "preferences.json"))
	if e != nil {
		return
	}
	var p struct {
		City     string `json:"city"`
		Timezone string `json:"timezone"`
	}
	if json.Unmarshal(b, &p) == nil {
		preferenceState.City = p.City
		if _, e := time.LoadLocation(p.Timezone); e == nil {
			preferenceState.Timezone = p.Timezone
		}
	}
}

// writeAtomicFile writes data to path atomically using a temporary file with Sync() and Rename().
func writeAtomicFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	_ = os.Remove(path + ".tmp")
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	cleaned := false
	defer func() {
		if !cleaned {
			_ = f.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err := f.Chmod(perm); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleaned = true
	_ = os.Remove(path + ".tmp")
	return nil
}

// SavePreferences saves city and timezone preferences atomically to preferences.json.
func SavePreferences(city, timezone string) error {
	b, err := json.Marshal(M{"city": city, "timezone": timezone})
	if err != nil {
		return err
	}
	return writeAtomicFile(filepath.Join(stateDirectory, "preferences.json"), b, 0600)
}

func spokenWeatherIntent(text string) bool {
	for _, word := range strings.FieldsFunc(strings.ToLower(strings.ReplaceAll(text, "ё", "е")), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= 'а' && r <= 'я') && !(r >= '0' && r <= '9')
	}) {
		switch {
		case strings.HasPrefix(word, "погод"),
			strings.HasPrefix(word, "температур"),
			strings.HasPrefix(word, "градус"),
			strings.HasPrefix(word, "прогноз"),
			strings.HasPrefix(word, "холодн"),
			strings.HasPrefix(word, "тепл"),
			strings.HasPrefix(word, "жарк"),
			strings.HasPrefix(word, "мороз"),
			strings.HasPrefix(word, "дожд"),
			strings.HasPrefix(word, "снег"),
			strings.HasPrefix(word, "снеж"),
			strings.HasPrefix(word, "ветер"),
			strings.HasPrefix(word, "ветр"),
			word == "улице", word == "окном", word == "зонт",
			word == "weather", word == "temperature", word == "forecast", word == "rain", word == "snow", word == "cold", word == "warm", word == "hot", word == "outside",
			word == "pocasie", word == "počasie", word == "teplota":
			return true
		}
	}
	return false
}

func spokenTimeIntent(text string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(text, "ё", "е"))
	// "Сколько" alone is too broad: "сколько стоит" is not a clock request.
	if strings.Contains(normalized, "сколько врем") || strings.Contains(normalized, "сколько час") ||
		strings.Contains(normalized, "который час") || strings.Contains(normalized, "what time") ||
		strings.Contains(normalized, "qué hora") || strings.Contains(normalized, "que hora") ||
		strings.Contains(normalized, "cuánto tiempo") || strings.Contains(normalized, "cuanto tiempo") {
		return true
	}
	for _, word := range strings.FieldsFunc(normalized, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= 'а' && r <= 'я') && !(r >= '0' && r <= '9')
	}) {
		switch {
		case strings.HasPrefix(word, "врем"),
			strings.HasPrefix(word, "час"),
			strings.HasPrefix(word, "котор"),
			strings.HasPrefix(word, "дат"),
			strings.HasPrefix(word, "числ"),
			strings.HasPrefix(word, "день"),
			strings.HasPrefix(word, "дня"),
			strings.HasPrefix(word, "сегодн"),
			word == "time", word == "clock", word == "date", word == "today", word == "day",
			word == "hora", word == "tiempo",
			word == "cas", word == "čas", word == "kolko", word == "koľko", word == "hodin", word == "hodín":
			return true
		}
	}
	return false
}

func spokenVolumeIntent(text string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(text, "ё", "е"))
	for _, word := range strings.FieldsFunc(normalized, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= 'а' && r <= 'я') && !(r >= '0' && r <= '9')
	}) {
		switch {
		case strings.HasPrefix(word, "громк"),
			strings.HasPrefix(word, "громч"),
			strings.HasPrefix(word, "тих"),
			strings.HasPrefix(word, "тиш"),
			strings.HasPrefix(word, "погромч"),
			strings.HasPrefix(word, "потиш"),
			strings.HasPrefix(word, "звук"),
			strings.HasPrefix(word, "уровен"),
			word == "volume", word == "louder", word == "quieter", word == "percent",
			word == "hlasitost", word == "hlasnejšie", word == "tichšie":
			return true
		}
	}
	return false
}

func setHomeCity(args M) M {
	city, _ := args["city"].(string)
	city = strings.TrimSpace(city)
	if city == "" || len(city) > 150 {
		return M{"ok": false, "error": "Укажи город для погоды по умолчанию."}
	}
	preferenceState.Lock()
	defer preferenceState.Unlock()
	if e := SavePreferences(city, preferenceState.Timezone); e != nil {
		return M{"ok": false, "error": "Не удалось сохранить город."}
	}
	preferenceState.City = city
	return M{"ok": true, "city": city}
}

func currentTime(args M) M {
	zone, _ := args["timezone"].(string)
	if zone == "" {
		preferenceState.Lock()
		zone = preferenceState.Timezone
		preferenceState.Unlock()
	}
	location, e := time.LoadLocation(zone)
	if e != nil {
		return M{"ok": false, "error": "Неизвестный часовой пояс. Используй IANA, например Europe/Moscow."}
	}
	now := time.Now().In(location)
	if now.Year() < 2024 {
		return M{"ok": false, "error": "Время колонки не синхронизировано; не называй выдуманное время."}
	}
	weekdays := []string{"воскресенье", "понедельник", "вторник", "среда", "четверг", "пятница", "суббота"}
	return M{"ok": true, "time": now.Format("15:04"), "date": now.Format("02.01.2006"), "weekday": weekdays[now.Weekday()], "timezone": zone, "iso": now.Format(time.RFC3339), "local_iso": now.Format("2006-01-02T15:04:05-07:00")}
}

var informationHTTP = &http.Client{Timeout: 9 * time.Second}

func readPublicJSON(endpoint string, out any) error {
	return readPublicJSONContext(context.Background(), endpoint, out)
}

type informationHTTPError struct {
	Status, Attempts int
	Reason           string
}

func (e *informationHTTPError) Error() string {
	if e.Status == 429 {
		return fmt.Sprintf("Open-Meteo ограничил частоту запросов (HTTP 429, попыток: %d). Попробуй позже.", e.Attempts)
	}
	if e.Status == 503 {
		return fmt.Sprintf("Open-Meteo сейчас перегружен (HTTP 503, попыток: %d). Погоду получить не удалось.", e.Attempts)
	}
	return fmt.Sprintf("Open-Meteo вернул HTTP %d (попыток: %d): %s", e.Status, e.Attempts, e.Reason)
}

func transientInformationStatus(status int) bool {
	return status == 429 || status == 500 || status == 502 || status == 503 || status == 504
}

func informationRetryDelay(header string, attempt int, now time.Time) (time.Duration, bool) {
	const maxDelay = 1500 * time.Millisecond
	if header != "" {
		var delay time.Duration
		if seconds, err := strconv.Atoi(header); err == nil && seconds >= 0 {
			if seconds > 1 {
				return 0, false
			}
			delay = time.Duration(seconds) * time.Second
		} else if until, err := http.ParseTime(header); err == nil {
			delay = until.Sub(now)
			if delay < 0 {
				delay = 0
			}
		} else {
			return time.Duration(attempt+1) * 250 * time.Millisecond, true
		}
		if delay > maxDelay {
			return 0, false
		}
		return delay, true
	}
	return time.Duration(attempt+1) * 250 * time.Millisecond, true
}

func readPublicJSONContext(parent context.Context, endpoint string, out any) error {
	ctx, cancel := context.WithTimeout(parent, 9*time.Second)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		response, err := informationHTTP.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("истекло время ожидания Open-Meteo или запрос отменён: %w", ctx.Err())
			}
			return fmt.Errorf("не удалось подключиться к Open-Meteo: %w", err)
		}
		if response.StatusCode == http.StatusOK {
			err = json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(out)
			response.Body.Close()
			if err != nil {
				return fmt.Errorf("Open-Meteo вернул некорректные данные: %w", err)
			}
			return nil
		}
		var detail struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 2048)).Decode(&detail)
		response.Body.Close()
		detail.Reason = strings.Join(strings.Fields(detail.Reason), " ")
		if len(detail.Reason) > 200 {
			detail.Reason = detail.Reason[:200]
		}
		statusErr := &informationHTTPError{Status: response.StatusCode, Attempts: attempt + 1, Reason: detail.Reason}
		if !transientInformationStatus(response.StatusCode) || attempt == 2 {
			return statusErr
		}
		delay, retry := informationRetryDelay(response.Header.Get("Retry-After"), attempt, time.Now())
		if !retry {
			return statusErr
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("запрос Open-Meteo отменён: %w", ctx.Err())
		case <-timer.C:
		}
	}
	return errors.New("не удалось получить ответ Open-Meteo")
}

func currentWeather(args M) M {
	return currentWeatherContext(context.Background(), args)
}

func currentWeatherContext(parent context.Context, args M) M {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	city, _ := args["city"].(string)
	if strings.TrimSpace(city) == "" {
		preferenceState.Lock()
		city = preferenceState.City
		preferenceState.Unlock()
	}
	if city == "" {
		return M{"ok": false, "error": "Город не указан; назови город или сохрани его командой set_home_city."}
	}
	if len(city) > 150 {
		return M{"ok": false, "error": "Слишком длинное название города."}
	}
	var places struct {
		Results []struct {
			Name, Country, Timezone string
			Latitude, Longitude     float64
		}
	}
	if e := readPublicJSONContext(ctx, "https://geocoding-api.open-meteo.com/v1/search?count=1&language=ru&name="+url.QueryEscape(city), &places); e != nil {
		return M{"ok": false, "error": e.Error()}
	}
	if len(places.Results) == 0 {
		return M{"ok": false, "error": "Город не найден; уточни название."}
	}
	p := places.Results[0]
	params := url.Values{"latitude": {fmt.Sprint(p.Latitude)}, "longitude": {fmt.Sprint(p.Longitude)}, "timezone": {"auto"}, "current": {"temperature_2m,apparent_temperature,wind_speed_10m,weather_code"}, "daily": {"temperature_2m_max,temperature_2m_min,precipitation_probability_max,weather_code"}, "forecast_days": {"3"}}
	var forecast struct {
		Current struct {
			Time        string
			Temperature float64 `json:"temperature_2m"`
			Feels       float64 `json:"apparent_temperature"`
			Wind        float64 `json:"wind_speed_10m"`
			Code        int     `json:"weather_code"`
		}
		Daily struct {
			Time []string
			Max  []float64 `json:"temperature_2m_max"`
			Min  []float64 `json:"temperature_2m_min"`
			Rain []int     `json:"precipitation_probability_max"`
			Code []int     `json:"weather_code"`
		}
	}
	primaryCtx, primaryCancel := context.WithTimeout(ctx, 6*time.Second)
	forecastError := readPublicJSONContext(primaryCtx, "https://api.open-meteo.com/v1/forecast?"+params.Encode(), &forecast)
	primaryCancel()
	if forecastError != nil {
		var statusErr *informationHTTPError
		var networkErr *url.Error
		mayFallback := (errors.As(forecastError, &statusErr) && statusErr.Status >= 500 && transientInformationStatus(statusErr.Status)) ||
			errors.As(forecastError, &networkErr) || errors.Is(forecastError, context.DeadlineExceeded)
		if !mayFallback || ctx.Err() != nil {
			return M{"ok": false, "error": forecastError.Error()}
		}
		// Independent public national weather service, not another path on the
		// overloaded Open-Meteo host. Do not use fallback to bypass HTTP429.
		if result, fallbackError := metNorwayWeather(ctx, p.Name, p.Country, p.Timezone, p.Latitude, p.Longitude); fallbackError == nil {
			return result
		} else {
			return M{"ok": false, "error": "Open-Meteo и резервная служба погоды недоступны: " + fallbackError.Error()}
		}
	}
	days := []M{}
	for i, date := range forecast.Daily.Time {
		if i < len(forecast.Daily.Max) && i < len(forecast.Daily.Min) && i < len(forecast.Daily.Rain) && i < len(forecast.Daily.Code) {
			days = append(days, M{"date": date, "min_c": math.Round(forecast.Daily.Min[i]), "max_c": math.Round(forecast.Daily.Max[i]), "precipitation_probability_percent": forecast.Daily.Rain[i], "condition": weatherDescription(forecast.Daily.Code[i])})
		}
	}
	return M{"ok": true, "city": p.Name, "country": p.Country, "timezone": p.Timezone, "observed_at": forecast.Current.Time, "temperature_c": math.Round(forecast.Current.Temperature), "feels_like_c": math.Round(forecast.Current.Feels), "wind_kmh": math.Round(forecast.Current.Wind), "condition": weatherDescription(forecast.Current.Code), "forecast": days, "source": "Open-Meteo"}
}

func weatherDescription(code int) string {
	switch {
	case code == 0:
		return "ясно"
	case code <= 2:
		return "переменная облачность"
	case code == 3:
		return "пасмурно"
	case code == 45 || code == 48:
		return "туман"
	case code >= 51 && code <= 57:
		return "морось"
	case code >= 61 && code <= 67:
		return "дождь"
	case code >= 71 && code <= 77:
		return "снег"
	case code >= 80 && code <= 82:
		return "ливень"
	case code == 85 || code == 86:
		return "снегопад"
	case code >= 95:
		return "гроза"
	default:
		return "уточняется"
	}
}
