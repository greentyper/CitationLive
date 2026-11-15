package main

// Current news headlines from trusted RSS feeds. No API key is required.
// Google News RSS is used for topic search; BBC Russian is available as a
// dedicated source. The model summarises the returned headlines in the user's language.

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type rssFeed struct {
	Channel struct {
		Title string `xml:"title"`
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			Description string `xml:"description"`
			PubDate     string `xml:"pubDate"`
			Source      string `xml:"source"`
		} `xml:"item"`
	} `xml:"channel"`
}

func newsToolDeclarations() []M {
	return []M{{
		"name":        "get_news",
		"description": "Get current news headlines from Google News or BBC Russian. Use for 'news', 'what's happening', 'news about X', 'play the news'. Do not invent headlines; summarise the returned items in the user's language and name the sources.",
		"parameters": M{
			"type": "OBJECT",
			"properties": M{
				"topic":    M{"type": "STRING", "description": "Optional topic, e.g. 'технологии', 'Украина', 'футбол'."},
				"source":   M{"type": "STRING", "enum": []string{"google", "bbc", "reuters"}, "description": "google (default), bbc, or reuters."},
				"limit":    M{"type": "INTEGER", "description": "Number of headlines, 1-10. Default 5."},
				"personal": M{"type": "BOOLEAN", "description": "Use the user's saved news topics (memory category news_topic) and merge Google News results."},
			},
		},
	}}
}

func newsFeedURL(source, topic string) (string, error) {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		source = "google"
	}
	topic = strings.TrimSpace(topic)
	switch source {
	case "google":
		if topic == "" {
			return "https://news.google.com/rss?hl=ru&gl=RU&ceid=RU:ru", nil
		}
		return "https://news.google.com/rss/search?q=" + url.QueryEscape(topic) + "&hl=ru&gl=RU&ceid=RU:ru", nil
	case "bbc":
		return "https://feeds.bbci.co.uk/russian/rss.xml", nil
	case "reuters":
		q := "site:reuters.com"
		if topic != "" {
			q += " " + topic
		}
		return "https://news.google.com/rss/search?q=" + url.QueryEscape(q) + "&hl=ru&gl=RU&ceid=RU:ru", nil
	default:
		return "", fmt.Errorf("unsupported news source %q", source)
	}
}

func getNews(ctx context.Context, topic, source string, limit int) (M, error) {
	feedURL, err := newsFeedURL(source, topic)
	if err != nil {
		return nil, err
	}
	return getNewsFromURL(ctx, feedURL, source, topic, limit)
}

func getNewsFromURL(ctx context.Context, feedURL, source, topic string, limit int) (M, error) {
	if limit <= 0 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", feedURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("user-agent", "Mozilla/5.0 (X11; Linux armv7l) CitationOne/1")
	req.Header.Set("accept", "application/rss+xml, application/xml, text/xml")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("news feed HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var feed rssFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("news feed parse: %w", err)
	}
	topicLower := strings.ToLower(strings.TrimSpace(topic))
	items := make([]M, 0, limit)
	seen := map[string]bool{}
	for _, item := range feed.Channel.Items {
		title := strings.TrimSpace(item.Title)
		if title == "" || seen[strings.ToLower(title)] {
			continue
		}
		description := cleanHTMLText(item.Description)
		if topicLower != "" && strings.EqualFold(source, "bbc") {
			haystack := strings.ToLower(title + " " + description)
			if !strings.Contains(haystack, topicLower) {
				continue
			}
		}
		if len([]rune(description)) > 300 {
			description = string([]rune(description)[:300])
		}
		seen[strings.ToLower(title)] = true
		itemSource := strings.TrimSpace(item.Source)
		if itemSource == "" {
			itemSource = strings.TrimSpace(feed.Channel.Title)
		}
		items = append(items, M{
			"title":       title,
			"source":      itemSource,
			"published":   strings.TrimSpace(item.PubDate),
			"link":        strings.TrimSpace(item.Link),
			"description": description,
		})
		if len(items) >= limit {
			break
		}
	}
	if len(items) == 0 {
		return M{"ok": false, "error": "no news items found", "topic": topic, "source": source}, nil
	}
	return M{
		"ok": true, "topic": topic, "source": source,
		"feed_title":  strings.TrimSpace(feed.Channel.Title),
		"items":       items,
		"instruction": "Перескажи эти новости по-русски, не выдумывай факты. Для 'включи новости' кратко назови 5 главных заголовков и источники.",
	}, nil
}

func personalNewsTopics() []string {
	var topics []string
	seen := map[string]bool{}
	for _, entry := range personalMemory.List() {
		if !strings.EqualFold(strings.TrimSpace(entry.Category), "news_topic") {
			continue
		}
		topic := strings.TrimSpace(entry.Text)
		if topic == "" || seen[strings.ToLower(topic)] {
			continue
		}
		seen[strings.ToLower(topic)] = true
		topics = append(topics, topic)
	}
	return topics
}

func getPersonalNews(ctx context.Context, limit int) (M, error) {
	// Account-linked Google News home first, when the owner imported cookies.
	if len(googleNewsCookies.Names()) > 0 {
		if res, err := getGoogleNewsHome(ctx, limit, true); err == nil {
			res["source"] = "google_news_account"
			res["personal"] = true
			res["topics"] = personalNewsTopics()
			return res, nil
		}
	}
	// Public Google News home is still a richer Google News surface than RSS
	// search and may be region-personalized.
	if res, err := getGoogleNewsHome(ctx, limit, false); err == nil {
		res["source"] = "google_news_home_public"
		res["personal"] = true
		res["topics"] = personalNewsTopics()
		return res, nil
	}
	topics := personalNewsTopics()
	if len(topics) == 0 {
		res, err := getNews(ctx, "", "google", limit)
		if err == nil {
			res["personal"] = true
			res["topics"] = []string{}
		}
		return res, err
	}
	if limit <= 0 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}
	var merged []M
	seen := map[string]bool{}
	for _, topic := range topics {
		res, err := getNews(ctx, topic, "google", limit)
		if err != nil || res["ok"] != true {
			continue
		}
		items, _ := res["items"].([]M)
		for _, item := range items {
			title, _ := item["title"].(string)
			key := strings.ToLower(strings.TrimSpace(title))
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, item)
			if len(merged) >= limit {
				break
			}
		}
		if len(merged) >= limit {
			break
		}
	}
	if len(merged) == 0 {
		return M{"ok": false, "error": "no personal news items found", "personal": true, "topics": topics}, nil
	}
	return M{
		"ok": true, "personal": true, "topics": topics, "source": "google",
		"items":       merged,
		"instruction": "Перескажи эти новости по-русски по темам пользователя, не выдумывай факты и назови источники.",
	}, nil
}

func handleNewsTool(ctx context.Context, name string, args M) (M, bool) {
	if name != "get_news" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	topic, _ := args["topic"].(string)
	source, _ := args["source"].(string)
	limit := 5
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}
	personal, _ := args["personal"].(bool)
	var res M
	var err error
	if personal {
		res, err = getPersonalNews(ctx, limit)
	} else {
		res, err = getNews(ctx, topic, source, limit)
	}
	if err != nil {
		return M{"ok": false, "error": clean(err)}, true
	}
	return res, true
}
