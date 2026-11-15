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
	"strings"
	"sync"
	"time"
)

// https://api.met.no/doc/locationforecast/HowTO
// Four-decimal coordinates and a descriptive User-Agent support upstream
// identification/caching. Operators can supply their public contact in the env.
const metWeatherUserAgent = "CitationVoice/1.0 (personal Harman-Kardon Citation ONE MKIII weather client)"

type metForecast struct {
	Properties struct {
		Meta struct {
			UpdatedAt time.Time `json:"updated_at"`
		} `json:"meta"`
		Timeseries []struct {
			Time time.Time `json:"time"`
			Data struct {
				Instant struct {
					Details struct {
						Temperature *float64 `json:"air_temperature"`
						Wind        *float64 `json:"wind_speed"`
					} `json:"details"`
				} `json:"instant"`
				Next1 struct {
					Summary struct {
						Symbol string `json:"symbol_code"`
					} `json:"summary"`
				} `json:"next_1_hours"`
				Next6 struct {
					Summary struct {
						Symbol string `json:"symbol_code"`
					} `json:"summary"`
				} `json:"next_6_hours"`
			} `json:"data"`
		} `json:"timeseries"`
	} `json:"properties"`
}

var metForecastCache = struct {
	sync.Mutex
	Entries map[string]struct {
		Data    metForecast
		Expires time.Time
	}
}{Entries: make(map[string]struct {
	Data    metForecast
	Expires time.Time
})}

func metNorwayWeather(parent context.Context, city, country, timezone string, latitude, longitude float64) (M, error) {
	ctx, cancel := context.WithTimeout(parent, 6*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	params := url.Values{"lat": {fmt.Sprintf("%.4f", latitude)}, "lon": {fmt.Sprintf("%.4f", longitude)}}
	endpoint := "https://api.met.no/weatherapi/locationforecast/2.0/compact?" + params.Encode()
	now := time.Now()
	metForecastCache.Lock()
	cached, found := metForecastCache.Entries[endpoint]
	metForecastCache.Unlock()
	forecast := cached.Data
	if !found || !now.Before(cached.Expires) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		userAgent := strings.TrimSpace(os.Getenv("CITATION_WEATHER_USER_AGENT"))
		if userAgent == "" {
			userAgent = metWeatherUserAgent
		}
		request.Header.Set("User-Agent", userAgent)
		request.Header.Set("Accept", "application/json")
		response, err := informationHTTP.Do(request)
		if err != nil {
			return nil, fmt.Errorf("MET Norway: %w", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("MET Norway вернул HTTP %d", response.StatusCode)
		}
		if err = json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&forecast); err != nil {
			return nil, fmt.Errorf("некорректные данные MET Norway: %w", err)
		}
		if _, err = metWeatherResult(forecast, city, country, timezone, now); err != nil {
			return nil, err
		}
		// Honor a shorter upstream expiry while never retaining more than 10min.
		expires := now.Add(10 * time.Minute)
		if upstream, err := http.ParseTime(response.Header.Get("Expires")); err == nil && upstream.Before(expires) {
			expires = upstream
		}
		metForecastCache.Lock()
		if len(metForecastCache.Entries) >= 16 {
			clear(metForecastCache.Entries)
		}
		metForecastCache.Entries[endpoint] = struct {
			Data    metForecast
			Expires time.Time
		}{forecast, expires}
		metForecastCache.Unlock()
	}
	return metWeatherResult(forecast, city, country, timezone, now)
}

func metWeatherResult(forecast metForecast, city, country, timezone string, now time.Time) (M, error) {
	updated := forecast.Properties.Meta.UpdatedAt
	if updated.IsZero() || now.Sub(updated) > 12*time.Hour || updated.After(now.Add(15*time.Minute)) {
		return nil, errors.New("MET Norway вернул устаревший или некорректный прогноз")
	}
	nearest, distance := -1, 2*time.Hour
	for i, point := range forecast.Properties.Timeseries {
		delta := point.Time.Sub(now)
		if delta < 0 {
			delta = -delta
		}
		if delta < distance {
			nearest, distance = i, delta
		}
	}
	if nearest < 0 {
		return nil, errors.New("в прогнозе MET Norway нет данных на текущий час")
	}
	point := forecast.Properties.Timeseries[nearest]
	details := point.Data.Instant.Details
	if details.Temperature == nil || details.Wind == nil || math.IsNaN(*details.Temperature) || math.IsInf(*details.Temperature, 0) || math.IsNaN(*details.Wind) || math.IsInf(*details.Wind, 0) || *details.Wind < 0 {
		return nil, errors.New("MET Norway не вернул температуру или ветер")
	}
	symbol := point.Data.Next1.Summary.Symbol
	if symbol == "" {
		symbol = point.Data.Next6.Summary.Symbol
	}
	// No invented feels-like temperature or daily aggregation: this fallback
	// explicitly provides only current-hour model temperature/wind/condition.
	return M{"ok": true, "city": city, "country": country, "timezone": timezone,
		"temperature_c": math.Round(*details.Temperature), "wind_kmh": math.Round(*details.Wind * 3.6),
		"condition": metWeatherDescription(symbol), "forecast_at": point.Time.Format(time.RFC3339),
		"updated_at": updated.Format(time.RFC3339), "data_kind": "model_forecast",
		"forecast": []M{}, "unavailable_fields": []string{"feels_like_c", "daily_forecast"},
		"source": "MET Norway", "source_url": "https://api.met.no/weatherapi/locationforecast/2.0/documentation"}, nil
}

func metWeatherDescription(symbol string) string {
	switch {
	case strings.Contains(symbol, "thunder"):
		return "гроза"
	case strings.Contains(symbol, "sleet"):
		return "мокрый снег"
	case strings.Contains(symbol, "snow"):
		return "снег"
	case strings.Contains(symbol, "rain"):
		return "дождь"
	case strings.Contains(symbol, "fog"):
		return "туман"
	case strings.HasPrefix(symbol, "clearsky"):
		return "ясно"
	case strings.HasPrefix(symbol, "fair"), strings.HasPrefix(symbol, "partlycloudy"):
		return "переменная облачность"
	case symbol == "cloudy":
		return "пасмурно"
	default:
		return "уточняется"
	}
}
