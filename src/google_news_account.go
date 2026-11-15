package main

// Optional account-linked Google News mode.
//
// Google has no public Google News API and no OAuth scope for the personalized
// "For you" feed. The web app embeds its personalized feed in
// AF_initDataCallback JSON, keyed to the signed-in account cookies. This file
// reads that HTML only when the user explicitly imports their own Google News
// cookies. It never logs cookie values, stores them 0600, and falls back to the
// public Google News RSS when cookies are absent, expired or blocked.
//
// This is an unofficial browser surface and can change without notice. Use it
// only for the account owner's personal reading.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const googleNewsCookieFileName = "google_news_cookies.json"

var googleNewsCookieName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

type googleNewsCookieStore struct {
	mu      sync.Mutex
	path    string
	cookies map[string]string
	loaded  bool
}

var googleNewsCookies = &googleNewsCookieStore{
	path: filepath.Join(stateDirectory, googleNewsCookieFileName),
}

func (s *googleNewsCookieStore) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var file struct {
		Cookies map[string]string `json:"cookies"`
	}
	if json.Unmarshal(data, &file) == nil {
		s.cookies = file.Cookies
	}
}

func (s *googleNewsCookieStore) Save(cookies map[string]string) (int, error) {
	clean := map[string]string{}
	for name, value := range cookies {
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name == "" || value == "" || !googleNewsCookieName.MatchString(name) {
			continue
		}
		if strings.ContainsAny(value, ";\r\n") {
			continue
		}
		clean[name] = value
	}
	if len(clean) == 0 {
		return 0, fmt.Errorf("no valid Google News cookies")
	}
	data, err := json.MarshalIndent(map[string]any{"cookies": clean}, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := writeAtomicFile(s.path, data, 0600); err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.cookies = clean
	s.loaded = true
	s.mu.Unlock()
	return len(clean), nil
}

func (s *googleNewsCookieStore) CookieHeader() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if len(s.cookies) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.cookies))
	for name, value := range s.cookies {
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; ")
}

func (s *googleNewsCookieStore) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	names := make([]string, 0, len(s.cookies))
	for name := range s.cookies {
		names = append(names, name)
	}
	return names
}

func (s *googleNewsCookieStore) Clear() error {
	s.mu.Lock()
	s.cookies = nil
	s.loaded = true
	s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func parseCookieHeader(header string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name != "" && value != "" && googleNewsCookieName.MatchString(name) {
			out[name] = value
		}
	}
	return out
}

// googleNewsHomeURL is localized for the user's main language.
func googleNewsHomeURL() string {
	return "https://news.google.com/home?hl=ru&gl=RU&ceid=RU:ru"
}

// googleNewsCookiesValid checks the browser account surface used by Chrome.
// It returns true only when the cookies resolve to a signed-in Google account.
func googleNewsCookiesValid(ctx context.Context, cookieHeader string) bool {
	if strings.TrimSpace(cookieHeader) == "" {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://accounts.google.com/ListAccounts?gpsia=1&source=ChromiumBrowser&json=standard", nil)
	if err != nil {
		return false
	}
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36")
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("x-requested-with", "XMLHttpRequest")
	req.Header.Set("origin", "https://accounts.google.com")
	req.Header.Set("referer", "https://accounts.google.com/")
	req.Header.Set("cookie", cookieHeader)
	client := &http.Client{Timeout: 12 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(body))
	// Current ListAccounts replies embed a postMessage with a "gaia.l.a" tuple
	// and the account email; older replies used JSON keys. Accept all shapes.
	return strings.Contains(lower, "gaia.l.a") ||
		strings.Contains(lower, "\"email\"") ||
		strings.Contains(lower, "obfuscatedgaiaid") ||
		strings.Contains(lower, "\"gaia\"")
}

func fetchGoogleNewsHome(ctx context.Context, cookies string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", googleNewsHomeURL(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36")
	req.Header.Set("accept-language", "ru-RU,ru;q=0.9,en;q=0.8")
	req.Header.Set("accept", "text/html,application/xhtml+xml")
	if cookies != "" {
		req.Header.Set("cookie", cookies)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("google news home HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func googleNewsStoryFromArray(arr []any) (M, bool) {
	if len(arr) < 7 || numberValue(arr[0]) != 13 {
		return nil, false
	}
	title, _ := arr[2].(string)
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, false
	}
	link, _ := arr[6].(string)
	link = strings.TrimSpace(link)
	if !strings.HasPrefix(link, "http") {
		return nil, false
	}
	published := ""
	if ts, ok := arr[4].([]any); ok && len(ts) > 0 {
		if sec := int64(numberValue(ts[0])); sec > 0 {
			published = time.Unix(sec, 0).UTC().Format(time.RFC3339)
		}
	}
	source := googleNewsPublisher(arr)
	if source == "" {
		if u, err := url.Parse(link); err == nil {
			source = u.Hostname()
		}
	}
	return M{
		"title": title, "link": link, "source": source,
		"published": published,
	}, true
}

func numberValue(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

func googleNewsPublisher(arr []any) string {
	var find func(any) string
	find = func(v any) string {
		switch x := v.(type) {
		case []any:
			if len(x) >= 3 && numberValue(x[0]) == 12 {
				if name, _ := x[2].(string); strings.TrimSpace(name) != "" {
					return strings.TrimSpace(name)
				}
			}
			for _, e := range x {
				if name := find(e); name != "" {
					return name
				}
			}
		}
		return ""
	}
	return find(arr)
}

func walkGoogleNewsData(v any, out *[]M, seen map[string]bool) {
	switch x := v.(type) {
	case []any:
		if item, ok := googleNewsStoryFromArray(x); ok {
			key := strings.ToLower(item["title"].(string))
			if !seen[key] {
				seen[key] = true
				*out = append(*out, item)
			}
		}
		for _, e := range x {
			walkGoogleNewsData(e, out, seen)
		}
	case map[string]any:
		for _, e := range x {
			walkGoogleNewsData(e, out, seen)
		}
	}
}

func parseGoogleNewsHTML(html string) []M {
	var out []M
	seen := map[string]bool{}
	search := 0
	for {
		idx := strings.Index(html[search:], "AF_initDataCallback(")
		if idx < 0 {
			break
		}
		idx += search
		dataIdx := strings.Index(html[idx:], "data:")
		if dataIdx < 0 {
			search = idx + 1
			continue
		}
		dataIdx += idx + len("data:")
		dec := json.NewDecoder(strings.NewReader(html[dataIdx:]))
		dec.UseNumber()
		var value any
		if err := dec.Decode(&value); err == nil {
			walkGoogleNewsData(value, &out, seen)
		}
		search = idx + len("AF_initDataCallback(")
		if len(out) >= 20 {
			break
		}
	}
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

func getGoogleNewsHome(ctx context.Context, limit int, useCookies bool) (M, error) {
	if limit <= 0 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}
	cookies := ""
	if useCookies {
		cookies = googleNewsCookies.CookieHeader()
		if !googleNewsCookiesValid(ctx, cookies) {
			return nil, fmt.Errorf("google news account cookies are not valid or expired")
		}
	}
	html, err := fetchGoogleNewsHome(ctx, cookies)
	if err != nil {
		return nil, err
	}
	items := parseGoogleNewsHTML(html)
	if len(items) == 0 {
		return nil, fmt.Errorf("google news home contained no stories")
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return M{
		"ok": true, "personal": useCookies, "source": "google_news_home",
		"items":       items,
		"instruction": "Перескажи эти новости по-русски, не выдумывай факты и назови источники.",
	}, nil
}
