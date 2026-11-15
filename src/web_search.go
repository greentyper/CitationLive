package main

// Real-time web search and deterministic unit conversion for Gemini Live.
//
// The Live session cannot combine native Google Search grounding with the
// function-calling tools used for timers/calendar/music in the same setup
// request. Instead, the model calls a custom `web_search` function and this
// bridge performs a separate non-Live Gemini request with Google Search
// grounding. The result is returned as a normal tool response.
//
// The Gemini API key is the same key used for the Live session; it is never
// logged. The search endpoint is a variable so tests can use httptest.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
)

var geminiSearchEndpoint = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s"

// Try current non-Live models in order. The Live model name is not valid for
// generateContent. If a model is unavailable, the next one is tried.
var geminiSearchModels = []string{
	"gemini-3.8-flash",
}

var geminiSearchClient = &http.Client{Timeout: 12 * time.Second}

func searchToolDeclarations() []M {
	return []M{
		{
			"name":        "web_search",
			"description": "Search the live web through Bing for current facts, news, prices, sports, weather, laws, or anything that may have changed. Always pass the query in Russian; do not translate it and do not discard the user's key words. Do not use for currency conversion; use convert_currency. The result contains one ready-to-speak field `answer`. Say `answer` once, close to its wording, then stop: do not repeat its quote, do not paraphrase it afterwards and do not name the source a second time. If `ok` is false, follow `instruction`. Do not read URLs.",
			"parameters": M{
				"type": "OBJECT",
				"properties": M{
					"query": M{"type": "STRING", "description": "Natural-language search query, in the user's language."},
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "convert_units",
			"description": "Convert a numeric value between units: length, mass, temperature, volume, speed, time, and data. For currency exchange rates use convert_currency.",
			"parameters": M{
				"type": "OBJECT",
				"properties": M{
					"value": M{"type": "NUMBER", "description": "Numeric value to convert."},
					"from":  M{"type": "STRING", "description": "Source unit, e.g. km, mi, kg, lb, c, f, l, gal, km/h, mph, s, h, mb, gb."},
					"to":    M{"type": "STRING", "description": "Target unit."},
				},
				"required": []string{"value", "from", "to"},
			},
		},
		{
			"name":        "convert_currency",
			"description": "Convert an amount between currencies using live exchange rates. Use ISO codes such as USD, EUR, RUB, GBP, KZT.",
			"parameters": M{
				"type": "OBJECT",
				"properties": M{
					"value": M{"type": "NUMBER", "description": "Amount to convert."},
					"from":  M{"type": "STRING", "description": "Source ISO currency code, e.g. USD."},
					"to":    M{"type": "STRING", "description": "Target ISO currency code, e.g. RUB."},
				},
				"required": []string{"value", "from", "to"},
			},
		},
	}
}

func handleSearchTool(ctx context.Context, name string, args M) (M, bool) {
	switch name {
	case "web_search":
		// Bound the entire fallback chain, not each provider separately.
		// Otherwise four sequential network timeouts can leave the user
		// waiting close to a minute after the progress announcement.
		ctx, cancel := context.WithTimeout(ctx, 18*time.Second)
		defer cancel()
		query, _ := args["query"].(string)
		query = normalizeSearchQuery(query)
		if query == "" || len([]rune(query)) < 3 {
			return M{
				"ok": false, "error": "search query is too vague",
				"instruction": "Попроси уточнить, что именно нужно найти, и сформулируй короткий конкретный запрос по-русски.",
			}, true
		}
		result, bingErr := firstSearchSuccess(ctx, query, bingRSSSearch, bingSearch)
		if bingErr == nil {
			return result, true
		}
		result, fallbackErr := firstSearchSuccess(ctx, query, duckDuckGoSearch, wikipediaSearch)
		if fallbackErr == nil {
			return result, true
		}
		return M{
			"ok":          false,
			"error":       fmt.Sprintf("bing: %v; fallback: %v", bingErr, fallbackErr),
			"instruction": "Не повторяй поиск. Скажи, что поиск не дал результатов, и дай краткий ответ из общих знаний с оговоркой, что данные могут быть неактуальны.",
		}, true
	case "convert_units":
		result, err := convertUnits(args)
		if err != nil {
			return M{"ok": false, "error": clean(err)}, true
		}
		return result, true
	case "convert_currency":
		result, err := convertCurrency(ctx, args)
		if err != nil {
			return M{"ok": false, "error": clean(err)}, true
		}
		return result, true
	default:
		return nil, false
	}
}

var (
	ddgLinkRe     = regexp.MustCompile(`(?is)<a[^>]+class="result-link"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippetRe  = regexp.MustCompile(`(?is)<td[^>]+class="result-snippet"[^>]*>(.*?)</td>`)
	bingResultRe  = regexp.MustCompile(`(?is)<li class="b_algo".*?</li>`)
	bingTitleRe   = regexp.MustCompile(`(?is)<h2[^>]*><a[^>]*href="([^"]+)"[^>]*>(.*?)</a></h2>`)
	bingSnippetRe = regexp.MustCompile(`(?is)<div class="b_caption"[^>]*><p[^>]*>(.*?)</p>`)
	htmlTagRe     = regexp.MustCompile(`(?s)<[^>]+>`)
	digitRe       = regexp.MustCompile(`\d`)
)

// bingSearch uses the server-rendered Bing HTML endpoint, which works from
// the speaker without an API key. DuckDuckGo currently serves an anomaly
// challenge to this network, and Gemini generateContent is quota-limited.
func bingSearch(ctx context.Context, query string) (M, error) {
	endpoint := "https://www.bing.com/search?q=" + url.QueryEscape(query) + "&setlang=ru&mkt=ru-RU"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux armv7l) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36")
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9,en;q=0.8")
	res, err := geminiSearchClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("bing returned HTTP %d", res.StatusCode)
	}
	page := string(raw)
	blocks := bingResultRe.FindAllString(page, 8)
	if len(blocks) == 0 {
		return nil, fmt.Errorf("bing returned no results")
	}
	items := make([]searchSnippet, 0, len(blocks))
	for _, block := range blocks {
		titleMatch := bingTitleRe.FindStringSubmatch(block)
		if len(titleMatch) < 3 {
			continue
		}
		title := cleanHTMLText(titleMatch[2])
		uri := decodeBingURL(titleMatch[1])
		snippet := ""
		if sm := bingSnippetRe.FindStringSubmatch(block); len(sm) > 1 {
			snippet = cleanHTMLText(sm[1])
		}
		if title == "" && snippet == "" {
			continue
		}
		items = append(items, searchSnippet{Title: title, Snippet: snippet, URI: uri})
		if len(items) >= 5 {
			break
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("bing returned empty snippets")
	}
	return searchResultFromSnippets("bing", query, items), nil
}

type searchSnippet struct {
	Title   string
	Snippet string
	URI     string
}

func sourceDomain(uri string) string {
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Host), "www.")
}

func sourceName(item searchSnippet) string {
	host := sourceDomain(item.URI)
	switch host {
	case "ru.wikipedia.org":
		return "Википедия"
	case "ria.ru":
		return "РИА Новости"
	case "rbc.ru":
		return "РБК"
	case "kommersant.ru":
		return "Коммерсантъ"
	case "mk.ru":
		return "МК"
	case "gismeteo.ru":
		return "GISMETEO"
	case "yandex.ru":
		return "Яндекс"
	case "lenta.ru":
		return "Лента.ру"
	case "consultant.ru":
		return "КонсультантПлюс"
	case "garant.ru":
		return "Гарант"
	case "gosuslugi.ru":
		return "Госуслуги"
	case "pravo.gov.ru":
		return "Право.gov.ru"
	case "government.ru":
		return "Правительство РФ"
	case "mil.ru":
		return "Минобороны"
	case "mvd.ru":
		return "МВД"
	case "nalog.gov.ru":
		return "ФНС"
	case "cbr.ru":
		return "Банк России"
	}
	if host != "" {
		return host
	}
	return truncateRunes(cleanHTMLText(item.Title), 80)
}

func cleanSnippet(s string) string {
	s = cleanHTMLText(s)
	prefix := regexp.MustCompile(`(?i)^\s*\d+\s*(ч|час[а-я]*|мин[а-я]*|дн[а-я]*|день|дня|дней)\s+назад\s*[·\-–—:]?\s*`)
	s = prefix.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	for _, cut := range []string{" …", "…", " ..."} {
		s = strings.TrimSuffix(s, cut)
	}
	// Snippets often start mid-sentence with a conjunction. Make the quote
	// stand alone and start with a capital letter.
	s = regexp.MustCompile(`(?i)^(при этом|однако|кроме того,?|также|в этом случае)\s+`).ReplaceAllString(s, "")
	if s != "" {
		r := []rune(s)
		r[0] = []rune(strings.ToUpper(string(r[0])))[0]
		s = string(r)
	}
	// Bing snippets often append the author and photo credit after the actual
	// quote: "Дмитрий Воронин Фото: Maksim". Strip that tail.
	s = regexp.MustCompile(`(?i)\s+[А-ЯЁ][а-яё]+(?:\s+[А-ЯЁ][а-яё]+)?\s+Фото:.*$`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?i)\s+Фото:.*$`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?i)\s+(Автор|Источник|Читать далее|Подробнее):.*$`).ReplaceAllString(s, "")
	for _, suffix := range []string{" РИА", " РБК", " ТАСС", " Интерфакс", " Коммерсантъ"} {
		s = strings.TrimSuffix(s, suffix)
	}
	s = strings.TrimSpace(s)
	if s != "" {
		r := []rune(s)
		last := r[len(r)-1]
		if last != '.' && last != '!' && last != '?' && last != '…' {
			s += "."
		}
	}
	return s
}

// splitSentences keeps decimal points inside numbers and returns complete
// sentences. Snippets without punctuation yield one item.
func splitSentences(text string) []string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) == 0 {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '.', '!', '?', '…':
			if runes[i] == '.' && i > 0 && i+1 < len(runes) &&
				unicode.IsDigit(runes[i-1]) && unicode.IsDigit(runes[i+1]) {
				continue
			}
			end := i + 1
			for end < len(runes) && strings.ContainsRune("»”\"')]", runes[end]) {
				end++
			}
			if sentence := strings.TrimSpace(string(runes[start:end])); sentence != "" {
				out = append(out, sentence)
			}
			start = end
			i = end - 1
		}
	}
	if rest := strings.TrimSpace(string(runes[start:])); rest != "" {
		out = append(out, rest)
	}
	return out
}

func snippetScore(query string, item searchSnippet) int {
	text := strings.ToLower(item.Title + " " + item.Snippet)
	score := 0
	for _, term := range strings.Fields(strings.ToLower(query)) {
		if len([]rune(term)) < 3 {
			continue
		}
		if strings.Contains(text, term) {
			score++
		}
	}
	return score
}

func queryWantsNews(query string) bool {
	q := strings.ToLower(query)
	for _, word := range []string{
		"новост", "сказал", "заявил", "налог", "курс", "сегодня", "вчера",
		"произош", "событ", "цена", "цены", "стоит", "рынок", "погода",
	} {
		if strings.Contains(q, word) {
			return true
		}
	}
	return false
}

func itemScore(query string, item searchSnippet, newsIntent bool) int {
	score := snippetScore(query, item)
	switch sourceDomain(item.URI) {
	case "consultant.ru", "garant.ru", "gosuslugi.ru", "pravo.gov.ru",
		"government.ru", "mil.ru", "mvd.ru", "nalog.gov.ru", "cbr.ru":
		score += 8
	case "ria.ru", "rbc.ru", "kommersant.ru", "lenta.ru", "mk.ru",
		"tass.ru", "interfax.ru", "rg.ru", "iz.ru", "vedomosti.ru",
		"bfm.ru", "rtvi.com":
		score += 4
	case "ru.wikipedia.org":
		if newsIntent {
			score -= 3
		}
	}
	return score
}

func sourceRank(item searchSnippet) int {
	switch sourceDomain(item.URI) {
	case "consultant.ru", "garant.ru", "gosuslugi.ru", "pravo.gov.ru",
		"government.ru", "mil.ru", "mvd.ru", "nalog.gov.ru", "cbr.ru":
		return 6
	case "ria.ru", "rbc.ru", "kommersant.ru", "lenta.ru", "mk.ru",
		"tass.ru", "interfax.ru", "rg.ru", "iz.ru", "vedomosti.ru",
		"bfm.ru", "rtvi.com":
		return 3
	}
	return 0
}

func boilerplateSentence(s string) bool {
	lower := strings.ToLower(s)
	for _, phrase := range []string{
		"без обязательств", "на заданную дату", "официальные курсы валют",
		"читайте также", "подробнее", "перейти на сайт", "смотрите также",
		"узнайте больше", "в материале", "фото:", "источник:",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

type searchSentence struct {
	item  searchSnippet
	text  string
	score int
}

// rankSearchSentences scores complete sentences across the top results. The
// highest-ranked page often contains a boilerplate first sentence (for example
// the CBR page "установил следующие курсы..."), while another result carries
// the actual number. Choosing the best sentence keeps the spoken answer useful
// and gives the quote a source that actually matches the summary.
func rankSearchSentences(query string, items []searchSnippet, newsIntent bool) []searchSentence {
	var out []searchSentence
	for _, item := range items {
		sentences := splitSentences(cleanSnippet(item.Snippet))
		if len(sentences) == 0 {
			if title := cleanHTMLText(item.Title); title != "" {
				sentences = []string{title}
			}
		}
		rank := sourceRank(item)
		for _, sentence := range sentences {
			if len([]rune(sentence)) < 20 {
				continue
			}
			score := snippetScore(query, searchSnippet{Title: item.Title, Snippet: sentence})*2 + rank
			if digitRe.MatchString(sentence) {
				score += 2
			}
			if newsIntent && sourceDomain(item.URI) == "ru.wikipedia.org" {
				score -= 2
			}
			if boilerplateSentence(sentence) {
				score -= 6
			}
			out = append(out, searchSentence{item: item, text: sentence, score: score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].score > out[j].score
	})
	return out
}

func truncateQuote(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	cut := r[:n]
	for i := len(cut) - 1; i >= 80; i-- {
		if cut[i] == '.' || cut[i] == '!' || cut[i] == '?' {
			return strings.TrimSpace(string(cut[:i+1]))
		}
	}
	return strings.TrimSpace(string(cut)) + "…"
}

// searchResultFromSnippets builds a short, speakable summary and a single
// source quote. URLs are kept separately for diagnostics, never in answer.
func searchResultFromSnippets(backend, query string, items []searchSnippet) M {
	if len(items) == 0 {
		return M{"ok": false, "error": "search returned no results"}
	}
	newsIntent := queryWantsNews(query)
	ranked := append([]searchSnippet(nil), items...)
	sort.SliceStable(ranked, func(i, j int) bool {
		return itemScore(query, ranked[i], newsIntent) > itemScore(query, ranked[j], newsIntent)
	})
	best := ranked[0]
	if backend == "wikipedia" {
		// Wikipedia's own search ranking is better than our snippet score for
		// this fallback; use its first result instead of picking a movie or an
		// unrelated article that happens to contain a keyword.
		best = items[0]
	} else if len(ranked) > 1 && sourceDomain(best.URI) == "ru.wikipedia.org" {
		// A Wikipedia biography is usually a poor quote for a news, price or
		// "what did X say" query. Prefer the first non-Wikipedia result.
		for _, item := range ranked[1:] {
			if sourceDomain(item.URI) != "ru.wikipedia.org" {
				best = item
				break
			}
		}
	}
	// Speak the summary and the citation once. The previous implementation
	// used the same snippet for both, so the model said the same sentence,
	// named the source, and then repeated the sentence. Quote a different
	// sentence; when the source has only one, attribute it without quoting
	// the same sentence again.
	candidateItems := ranked
	if backend == "wikipedia" {
		candidateItems = []searchSnippet{best}
	}
	candidates := rankSearchSentences(query, candidateItems, newsIntent)
	summary := ""
	quote := ""
	if len(candidates) > 0 && candidates[0].score > 0 {
		top := candidates[0]
		best = top.item
		summary = truncateQuote(top.text, 320)
		for _, candidate := range candidates[1:] {
			if candidate.item.URI != top.item.URI {
				continue
			}
			if candidate.text == top.text ||
				strings.Contains(summary, candidate.text) ||
				strings.Contains(candidate.text, summary) {
				continue
			}
			if len([]rune(candidate.text)) < 40 {
				continue
			}
			quote = truncateQuote(candidate.text, 220)
			break
		}
	}
	if summary == "" {
		sentences := splitSentences(cleanSnippet(best.Snippet))
		if len(sentences) > 0 {
			summary = truncateQuote(sentences[0], 320)
		}
	}
	if summary == "" {
		summary = truncateRunes(cleanHTMLText(best.Title), 180)
	}
	source := sourceName(best)
	answer := "Кратко: " + summary
	if quote != "" {
		answer = strings.TrimSpace(answer + " Цитирую с сайта " + source + ": «" + quote + "»")
	} else {
		answer = strings.TrimSpace(answer + " Источник: " + source + ".")
	}
	result := M{
		"ok": true, "backend": backend, "summary": summary,
		"quote": quote, "source_title": source,
		"article_title": truncateRunes(cleanHTMLText(best.Title), 120),
		"answer":        answer,
	}
	sources := make([]M, 0, len(items))
	for _, item := range items {
		sources = append(sources, M{"title": item.Title, "uri": item.URI})
	}
	result["sources"] = sources
	return result
}

// stripAnswerURLs removes bare URL lines from fallback provider answers so the
// model cannot read them aloud. Source attribution stays in source_title.
func stripAnswerURLs(text string) string {
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "http://") ||
			strings.HasPrefix(trimmed, "https://") ||
			strings.HasPrefix(trimmed, "www.") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// compactSearchToolResponse is the payload the Live model receives. The full
// result keeps summary/quote/sources for /search/probe and diagnostics, but
// sending all of them to the model invited a second paraphrase and a repeated
// citation. One ready-to-speak answer is enough.
func compactSearchToolResponse(result M) M {
	if result == nil {
		return M{"ok": false, "error": "search failed"}
	}
	out := M{"ok": result["ok"]}
	if answer, ok := result["answer"].(string); ok && strings.TrimSpace(answer) != "" {
		out["answer"] = stripAnswerURLs(answer)
		// source_title is intentionally omitted: answer already contains the
		// attribution, and a second copy invites the model to name it twice.
		out["instruction"] = "Произнеси answer один раз, близко к тексту, и закончи. Не повторяй цитату, не пересказывай её после и не называй источник второй раз: в answer уже есть источник."
		return out
	}
	if err, ok := result["error"].(string); ok && err != "" {
		out["error"] = err
	}
	if instruction, ok := result["instruction"].(string); ok && instruction != "" {
		out["instruction"] = instruction
	}
	return out
}

func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

type bingRSSDocument struct {
	Channel struct {
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			Description string `xml:"description"`
		} `xml:"item"`
	} `xml:"channel"`
}

// bingRSSSearch is the robust Bing fallback when the HTML result markup
// changes. It still uses Bing and keeps the query in the user's language.
func bingRSSSearch(ctx context.Context, query string) (M, error) {
	endpoint := "https://www.bing.com/search?format=rss&q=" + url.QueryEscape(query) + "&setlang=ru&mkt=ru-RU&cc=ru"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux armv7l) AppleWebKit/537.36 Chrome/120 Safari/537.36")
	res, err := geminiSearchClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("bing rss returned HTTP %d", res.StatusCode)
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	var doc bingRSSDocument
	items := make([]searchSnippet, 0, 5)
	if err := xml.Unmarshal(raw, &doc); err == nil {
		for _, item := range doc.Channel.Items {
			title := cleanHTMLText(item.Title)
			snippet := cleanHTMLText(item.Description)
			if title == "" && snippet == "" {
				continue
			}
			items = append(items, searchSnippet{Title: title, Snippet: snippet, URI: strings.TrimSpace(item.Link)})
			if len(items) >= 5 {
				break
			}
		}
	}
	if len(items) == 0 {
		// Bing's RSS occasionally contains characters that encoding/xml
		// rejects. Parse the same feed with a tolerant regex fallback.
		items = parseBingRSSItems(string(raw))
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("bing rss returned no results")
	}
	return searchResultFromSnippets("bing", query, items), nil
}

func parseBingRSSItems(page string) []searchSnippet {
	itemRe := regexp.MustCompile(`(?is)<item>(.*?)</item>`)
	titleRe := regexp.MustCompile(`(?is)<title>(.*?)</title>`)
	linkRe := regexp.MustCompile(`(?is)<link>(.*?)</link>`)
	descRe := regexp.MustCompile(`(?is)<description>(.*?)</description>`)
	var items []searchSnippet
	for _, block := range itemRe.FindAllStringSubmatch(page, -1) {
		if len(block) < 2 {
			continue
		}
		body := block[1]
		title := ""
		if m := titleRe.FindStringSubmatch(body); len(m) > 1 {
			title = cleanHTMLText(m[1])
		}
		link := ""
		if m := linkRe.FindStringSubmatch(body); len(m) > 1 {
			link = strings.TrimSpace(html.UnescapeString(m[1]))
		}
		snippet := ""
		if m := descRe.FindStringSubmatch(body); len(m) > 1 {
			snippet = cleanHTMLText(m[1])
		}
		if title == "" && snippet == "" {
			continue
		}
		items = append(items, searchSnippet{Title: title, Snippet: snippet, URI: link})
		if len(items) >= 5 {
			break
		}
	}
	return items
}

func decodeBingURL(raw string) string {
	raw = html.UnescapeString(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	enc := u.Query().Get("u")
	if strings.HasPrefix(enc, "a1") {
		enc = enc[2:]
	}
	if enc != "" {
		if decoded, err := base64.RawURLEncoding.DecodeString(enc); err == nil {
			s := string(decoded)
			if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
				return s
			}
		}
	}
	return raw
}

// duckDuckGoSearch is the no-key fallback used when the Gemini search quota is
// exhausted or the API key is limited to Live sessions. It returns top result
// titles/snippets/links for the model to synthesize.
func duckDuckGoSearch(ctx context.Context, query string) (M, error) {
	endpoints := []string{
		"https://lite.duckduckgo.com/lite/?q=" + url.QueryEscape(query),
		"https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query),
	}
	var lastErr error
	for _, endpoint := range endpoints {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux armv7l) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36")
		req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9,en;q=0.8")
		res, err := geminiSearchClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(res.Body, 2<<20))
		res.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			lastErr = fmt.Errorf("duckduckgo returned HTTP %d", res.StatusCode)
			continue
		}
		page := string(raw)
		links := ddgLinkRe.FindAllStringSubmatch(page, 6)
		snippets := ddgSnippetRe.FindAllStringSubmatch(page, 6)
		if len(links) == 0 {
			lastErr = fmt.Errorf("duckduckgo returned no results")
			continue
		}
		var answer strings.Builder
		sources := make([]M, 0, len(links))
		for i, link := range links {
			title := cleanHTMLText(link[2])
			uri := strings.TrimSpace(link[1])
			if strings.HasPrefix(uri, "//") {
				uri = "https:" + uri
			}
			snippet := ""
			if i < len(snippets) {
				snippet = cleanHTMLText(snippets[i][1])
			}
			if title == "" && snippet == "" {
				continue
			}
			if answer.Len() > 0 {
				answer.WriteString("\n")
			}
			answer.WriteString(title)
			if snippet != "" {
				answer.WriteString(" — ")
				answer.WriteString(snippet)
			}
			if uri != "" {
				answer.WriteString("\n")
				answer.WriteString(uri)
			}
			if uri != "" {
				sources = append(sources, M{"title": title, "uri": uri})
			}
			if len(sources) >= 5 {
				break
			}
		}
		text := strings.TrimSpace(answer.String())
		if text == "" {
			lastErr = fmt.Errorf("duckduckgo returned empty snippets")
			continue
		}
		if len(text) > 3000 {
			text = text[:3000]
		}
		result := M{"ok": true, "answer": text, "backend": "duckduckgo"}
		if len(sources) > 0 {
			result["sources"] = sources
		}
		return result, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("duckduckgo unavailable")
	}
	return nil, lastErr
}

func normalizeSearchQuery(s string) string {
	// Never discard words from the user's question. Only normalise whitespace;
	// the model is responsible for forming a concise search query.
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

type wikipediaSearchResponse struct {
	Query struct {
		Search []struct {
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		} `json:"search"`
	} `json:"query"`
}

// wikipediaSearch is the last no-key fallback for queries Bing refuses to
// answer (for example sensitive legal/conscription questions). It returns a
// normal summary/quote result and is clearly labelled as Wikipedia.
func wikipediaSearch(ctx context.Context, query string) (M, error) {
	endpoint := "https://ru.wikipedia.org/w/api.php?action=query&list=search&srsearch=" +
		url.QueryEscape(query) + "&format=json&utf8=1&srlimit=5"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "citation-one-gemini/1")
	res, err := geminiSearchClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("wikipedia returned HTTP %d", res.StatusCode)
	}
	var doc wikipediaSearchResponse
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	items := make([]searchSnippet, 0, len(doc.Query.Search))
	for _, item := range doc.Query.Search {
		title := cleanHTMLText(item.Title)
		snippet := cleanHTMLText(item.Snippet)
		if title == "" && snippet == "" {
			continue
		}
		uri := "https://ru.wikipedia.org/wiki/" + url.PathEscape(strings.ReplaceAll(title, " ", "_"))
		items = append(items, searchSnippet{Title: title, Snippet: snippet, URI: uri})
		if len(items) >= 5 {
			break
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("wikipedia returned no results")
	}
	if extract, err := wikipediaExtract(ctx, items[0].Title); err == nil {
		if sentence := chooseWikipediaSentence(extract, query); sentence != "" {
			items[0].Snippet = sentence
		}
	}
	return searchResultFromSnippets("wikipedia", query, items), nil
}

type wikipediaExtractResponse struct {
	Query struct {
		Pages map[string]struct {
			Extract string `json:"extract"`
		} `json:"pages"`
	} `json:"query"`
}

func wikipediaExtract(ctx context.Context, title string) (string, error) {
	endpoint := "https://ru.wikipedia.org/w/api.php?action=query&prop=extracts&explaintext=1&exsectionformat=plain&titles=" +
		url.QueryEscape(title) + "&format=json&utf8=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "citation-one-gemini/1")
	res, err := geminiSearchClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return "", err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("wikipedia extract HTTP %d", res.StatusCode)
	}
	var doc wikipediaExtractResponse
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	for _, page := range doc.Query.Pages {
		if strings.TrimSpace(page.Extract) != "" {
			return page.Extract, nil
		}
	}
	return "", fmt.Errorf("wikipedia extract empty")
}

func chooseWikipediaSentence(text, query string) string {
	text = cleanHTMLText(text)
	sentences := regexp.MustCompile(`(?s)[^.!?]+[.!?]`).FindAllString(text, -1)
	best := ""
	bestScore := -1
	for _, sentence := range sentences {
		sentence = strings.TrimSpace(sentence)
		if len([]rune(sentence)) < 40 {
			continue
		}
		score := snippetScore(query, searchSnippet{Snippet: sentence})
		if score > bestScore {
			best, bestScore = sentence, score
		}
	}
	return truncateQuote(best, 260)
}

func cleanHTMLText(s string) string {
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

type currencyCacheEntry struct {
	rates  map[string]float64
	expiry time.Time
}

var (
	currencyCacheMu sync.Mutex
	currencyCache   = map[string]currencyCacheEntry{}
)

func convertCurrency(ctx context.Context, args M) (M, error) {
	value, ok := args["value"].(float64)
	if !ok {
		return nil, fmt.Errorf("value must be a number")
	}
	from, _ := args["from"].(string)
	to, _ := args["to"].(string)
	from = strings.ToUpper(strings.TrimSpace(from))
	to = strings.ToUpper(strings.TrimSpace(to))
	if len(from) != 3 || len(to) != 3 {
		return nil, fmt.Errorf("currency codes must be 3 letters, e.g. USD and RUB")
	}
	if from == to {
		return M{"ok": true, "value": round6(value), "rate": 1.0, "from": from, "to": to, "formatted": fmt.Sprintf("%g %s", round6(value), to)}, nil
	}
	rate, err := currencyRate(ctx, from, to)
	if err != nil {
		return nil, err
	}
	result := value * rate
	return M{"ok": true, "value": round6(result), "rate": rate, "from": from, "to": to, "formatted": fmt.Sprintf("%g %s", round6(result), to)}, nil
}

func currencyRate(ctx context.Context, from, to string) (float64, error) {
	currencyCacheMu.Lock()
	if entry, ok := currencyCache[from]; ok && time.Now().Before(entry.expiry) {
		if rate, ok := entry.rates[to]; ok {
			currencyCacheMu.Unlock()
			return rate, nil
		}
	}
	currencyCacheMu.Unlock()

	rates, err := fetchCurrencyRates(ctx, from)
	if err != nil {
		return 0, err
	}
	rate, ok := rates[to]
	if !ok {
		return 0, fmt.Errorf("currency %s not supported", to)
	}
	currencyCacheMu.Lock()
	currencyCache[from] = currencyCacheEntry{rates: rates, expiry: time.Now().Add(10 * time.Minute)}
	currencyCacheMu.Unlock()
	return rate, nil
}

func fetchCurrencyRates(ctx context.Context, from string) (map[string]float64, error) {
	// Primary: open.er-api.com, no key, broad currency list.
	endpoint := "https://open.er-api.com/v6/latest/" + url.PathEscape(from)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err == nil {
		res, e := geminiSearchClient.Do(req)
		if e == nil {
			raw, readErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
			res.Body.Close()
			if readErr == nil && res.StatusCode >= 200 && res.StatusCode < 300 {
				var out struct {
					Result string             `json:"result"`
					Rates  map[string]float64 `json:"rates"`
				}
				if json.Unmarshal(raw, &out) == nil && out.Result == "success" && len(out.Rates) > 0 {
					return out.Rates, nil
				}
			}
		}
	}
	// Fallback: frankfurter.app.
	endpoint = "https://api.frankfurter.app/latest?from=" + url.QueryEscape(from)
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	res, err := geminiSearchClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("currency API returned HTTP %d", res.StatusCode)
	}
	var out struct {
		Rates map[string]float64 `json:"rates"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Rates) == 0 {
		return nil, fmt.Errorf("currency API returned no rates")
	}
	return out.Rates, nil
}

const geminiLiveSearchEndpoint = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

// geminiLiveSearch opens a short, separate Live session with only the native
// Google Search grounding tool. The main session keeps functionDeclarations;
// this avoids the Live API restriction that search tools and non-search tools
// cannot be combined in one setup.
func geminiLiveSearch(ctx context.Context, query string) (M, error) {
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("Gemini API key unavailable")
	}
	model := cfg.Model
	if model == "" {
		model = "models/gemini-3.8-live"
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: 6 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
	}
	var lastErr error
	for _, toolKey := range []string{"googleSearch", "google_search"} {
		c, _, err := dialer.DialContext(ctx, geminiLiveSearchEndpoint, http.Header{"X-Goog-Api-Key": []string{key}})
		if err != nil {
			lastErr = err
			continue
		}
		c.SetReadLimit(4 << 20)
		c.SetReadDeadline(time.Now().Add(20 * time.Second))
		setup := M{"setup": M{
			"model":             model,
			"generationConfig":  M{"responseModalities": []string{"TEXT"}},
			"tools":             []M{{toolKey: M{}}},
			"systemInstruction": M{"parts": []M{{"text": "Search the web and answer the user's question concisely with current facts. Include source names when available."}}},
		}}
		if err := c.WriteJSON(setup); err != nil {
			c.Close()
			lastErr = err
			continue
		}
		setupOK := false
		for i := 0; i < 12; i++ {
			var msg M
			if err := c.ReadJSON(&msg); err != nil {
				lastErr = err
				break
			}
			if _, ok := msg["setupComplete"]; ok {
				setupOK = true
				break
			}
			if e, ok := msg["error"]; ok {
				lastErr = fmt.Errorf("live search setup: %v", e)
				break
			}
		}
		if !setupOK {
			c.Close()
			continue
		}
		if err := c.WriteJSON(M{"clientContent": M{
			"turns":        []M{{"role": "user", "parts": []M{{"text": query}}}},
			"turnComplete": true,
		}}); err != nil {
			c.Close()
			lastErr = err
			continue
		}
		var answer strings.Builder
		deadline := time.Now().Add(18 * time.Second)
		for time.Now().Before(deadline) {
			var msg M
			if err := c.ReadJSON(&msg); err != nil {
				break
			}
			if e, ok := msg["error"]; ok {
				lastErr = fmt.Errorf("live search: %v", e)
				break
			}
			sc, _ := msg["serverContent"].(map[string]any)
			if sc == nil {
				continue
			}
			if mt, ok := sc["modelTurn"].(map[string]any); ok {
				if parts, ok := mt["parts"].([]any); ok {
					for _, p := range parts {
						part, _ := p.(map[string]any)
						if t, ok := part["text"].(string); ok {
							answer.WriteString(t)
						}
					}
				}
			}
			if ot, ok := sc["outputTranscription"].(map[string]any); ok {
				if t, ok := ot["text"].(string); ok {
					answer.WriteString(t)
				}
			}
			if tc, ok := sc["turnComplete"].(bool); ok && tc {
				break
			}
		}
		c.Close()
		text := strings.TrimSpace(answer.String())
		if text != "" {
			if len(text) > 3000 {
				text = text[:3000]
			}
			return M{"ok": true, "answer": text, "model": model, "backend": "live_google_search"}, nil
		}
		lastErr = fmt.Errorf("live search returned no text")
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("live search unavailable")
	}
	return nil, lastErr
}

// geminiGroundedSearch performs a non-Live generateContent request with the
// Google Search grounding tool. The Live API cannot combine this tool with
// functionDeclarations in the same setup.
func geminiGroundedSearch(ctx context.Context, query string) (M, error) {
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("Gemini API key unavailable")
	}
	payload := M{
		"contents": []M{{"parts": []M{{"text": query}}}},
		"tools":    []M{{"googleSearch": M{}}},
		"generationConfig": M{
			"temperature":     0.2,
			"maxOutputTokens": 1024,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, model := range geminiSearchModels {
		url := fmt.Sprintf(geminiSearchEndpoint, model, key)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("content-type", "application/json")
		res, err := geminiSearchClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			detail := strings.TrimSpace(string(raw))
			if len(detail) > 300 {
				detail = detail[:300]
			}
			lastErr = fmt.Errorf("search model %s returned HTTP %d: %s", model, res.StatusCode, detail)
			continue
		}
		var out struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
				GroundingMetadata struct {
					GroundingChunks []struct {
						Web struct {
							URI   string `json:"uri"`
							Title string `json:"title"`
						} `json:"web"`
					} `json:"groundingChunks"`
				} `json:"groundingMetadata"`
			} `json:"candidates"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			lastErr = err
			continue
		}
		if out.Error.Message != "" {
			lastErr = fmt.Errorf("search model %s: %s", model, out.Error.Message)
			continue
		}
		if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 {
			lastErr = fmt.Errorf("search model %s returned no answer", model)
			continue
		}
		var answer strings.Builder
		for _, part := range out.Candidates[0].Content.Parts {
			if part.Text != "" {
				if answer.Len() > 0 {
					answer.WriteString("\n")
				}
				answer.WriteString(part.Text)
			}
		}
		text := strings.TrimSpace(answer.String())
		if text == "" {
			lastErr = fmt.Errorf("search model %s returned an empty answer", model)
			continue
		}
		if len(text) > 3000 {
			text = text[:3000]
		}
		sources := make([]M, 0, 4)
		seen := map[string]bool{}
		for _, chunk := range out.Candidates[0].GroundingMetadata.GroundingChunks {
			uri := strings.TrimSpace(chunk.Web.URI)
			if uri == "" || seen[uri] {
				continue
			}
			seen[uri] = true
			sources = append(sources, M{"title": chunk.Web.Title, "uri": uri})
			if len(sources) >= 4 {
				break
			}
		}
		result := M{"ok": true, "answer": text, "model": model}
		if len(sources) > 0 {
			result["sources"] = sources
		}
		return result, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no search model available")
	}
	return nil, lastErr
}

type unitDef struct {
	factor float64
	base   string
}

var unitTable = map[string]unitDef{
	// length -> meter
	"m": {1, "length"}, "meter": {1, "length"}, "meters": {1, "length"},
	"km": {1000, "length"}, "kilometer": {1000, "length"}, "kilometers": {1000, "length"},
	"cm": {0.01, "length"}, "mm": {0.001, "length"},
	"mi": {1609.344, "length"}, "mile": {1609.344, "length"}, "miles": {1609.344, "length"},
	"ft": {0.3048, "length"}, "foot": {0.3048, "length"}, "feet": {0.3048, "length"},
	"in": {0.0254, "length"}, "inch": {0.0254, "length"}, "inches": {0.0254, "length"},
	"yd": {0.9144, "length"}, "yard": {0.9144, "length"}, "yards": {0.9144, "length"},
	// mass -> kilogram
	"kg": {1, "mass"}, "kilogram": {1, "mass"}, "kilograms": {1, "mass"},
	"g": {0.001, "mass"}, "gram": {0.001, "mass"}, "grams": {0.001, "mass"},
	"mg": {1e-6, "mass"},
	"lb": {0.45359237, "mass"}, "lbs": {0.45359237, "mass"}, "pound": {0.45359237, "mass"}, "pounds": {0.45359237, "mass"},
	"oz": {0.028349523125, "mass"}, "ounce": {0.028349523125, "mass"}, "ounces": {0.028349523125, "mass"},
	// volume -> liter
	"l": {1, "volume"}, "liter": {1, "volume"}, "liters": {1, "volume"}, "litre": {1, "volume"}, "litres": {1, "volume"},
	"ml": {0.001, "volume"}, "milliliter": {0.001, "volume"}, "milliliters": {0.001, "volume"},
	"gal": {3.785411784, "volume"}, "gallon": {3.785411784, "volume"}, "gallons": {3.785411784, "volume"},
	"qt": {0.946352946, "volume"}, "quart": {0.946352946, "volume"}, "quarts": {0.946352946, "volume"},
	"pt": {0.473176473, "volume"}, "pint": {0.473176473, "volume"}, "pints": {0.473176473, "volume"},
	"cup": {0.2365882365, "volume"}, "cups": {0.2365882365, "volume"},
	"floz": {0.0295735295625, "volume"}, "fl oz": {0.0295735295625, "volume"},
	// speed -> meter/second
	"m/s": {1, "speed"}, "mps": {1, "speed"},
	"km/h": {0.2777777778, "speed"}, "kph": {0.2777777778, "speed"}, "kmh": {0.2777777778, "speed"},
	"mph": {0.44704, "speed"},
	"kn":  {0.5144444444, "speed"}, "knot": {0.5144444444, "speed"}, "knots": {0.5144444444, "speed"},
	// time -> second
	"s": {1, "time"}, "sec": {1, "time"}, "second": {1, "time"}, "seconds": {1, "time"},
	"min": {60, "time"}, "minute": {60, "time"}, "minutes": {60, "time"},
	"h": {3600, "time"}, "hr": {3600, "time"}, "hour": {3600, "time"}, "hours": {3600, "time"},
	"day": {86400, "time"}, "days": {86400, "time"},
	// data -> byte
	"b": {1, "data"}, "byte": {1, "data"}, "bytes": {1, "data"},
	"kb": {1000, "data"}, "mb": {1e6, "data"}, "gb": {1e9, "data"}, "tb": {1e12, "data"},
	"kib": {1024, "data"}, "mib": {1048576, "data"}, "gib": {1073741824, "data"},
}

func normalizeUnit(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "°", "")
	s = strings.ReplaceAll(s, " ", "")
	switch s {
	case "c", "celsius", "цельсий", "цельсия":
		return "c"
	case "f", "fahrenheit", "фаренгейт", "фаренгейта":
		return "f"
	case "k", "kelvin", "кельвин", "кельвина":
		return "k"
	}
	return s
}

func convertUnits(args M) (M, error) {
	value, ok := args["value"].(float64)
	if !ok {
		if raw, ok := args["value"].(string); ok {
			parsed, err := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(raw, ",", ".")), 64)
			if err != nil {
				return nil, fmt.Errorf("value must be a number")
			}
			value = parsed
		} else {
			return nil, fmt.Errorf("value must be a number")
		}
	}
	fromRaw, _ := args["from"].(string)
	toRaw, _ := args["to"].(string)
	from := normalizeUnit(fromRaw)
	to := normalizeUnit(toRaw)
	if from == "" || to == "" {
		return nil, fmt.Errorf("from and to units are required")
	}

	// Temperature is affine, not a simple factor.
	if isTemp(from) || isTemp(to) {
		if !isTemp(from) || !isTemp(to) {
			return nil, fmt.Errorf("temperature conversion needs c, f or k on both sides")
		}
		result := convertTemperature(value, from, to)
		return M{"ok": true, "value": round6(result), "from": fromRaw, "to": toRaw, "formatted": fmt.Sprintf("%g %s", round6(result), toRaw)}, nil
	}

	fromDef, okFrom := unitTable[from]
	toDef, okTo := unitTable[to]
	if !okFrom || !okTo {
		return nil, fmt.Errorf("unsupported unit: %s -> %s", fromRaw, toRaw)
	}
	if fromDef.base != toDef.base {
		return nil, fmt.Errorf("incompatible units: %s (%s) and %s (%s)", fromRaw, fromDef.base, toRaw, toDef.base)
	}
	result := value * fromDef.factor / toDef.factor
	return M{"ok": true, "value": round6(result), "from": fromRaw, "to": toRaw, "formatted": fmt.Sprintf("%g %s", round6(result), toRaw)}, nil
}

func isTemp(u string) bool { return u == "c" || u == "f" || u == "k" }

func convertTemperature(v float64, from, to string) float64 {
	// to Celsius first
	var c float64
	switch from {
	case "c":
		c = v
	case "f":
		c = (v - 32) * 5 / 9
	case "k":
		c = v - 273.15
	}
	switch to {
	case "c":
		return c
	case "f":
		return c*9/5 + 32
	case "k":
		return c + 273.15
	}
	return c
}

func round6(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	return math.Round(v*1e6) / 1e6
}
