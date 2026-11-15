package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConvertUnitsLengthAndMass(t *testing.T) {
	cases := []struct {
		value    float64
		from, to string
		want     float64
	}{
		{1, "km", "m", 1000},
		{1, "mi", "km", 1.609344},
		{1, "kg", "lb", 2.2046226218},
		{1000, "g", "kg", 1},
	}
	for _, tc := range cases {
		res, err := convertUnits(M{"value": tc.value, "from": tc.from, "to": tc.to})
		if err != nil {
			t.Fatalf("%v %s->%s: %v", tc.value, tc.from, tc.to, err)
		}
		got, _ := res["value"].(float64)
		if diff := got - tc.want; diff > 1e-6 || diff < -1e-6 {
			t.Fatalf("%v %s->%s = %v, want %v", tc.value, tc.from, tc.to, got, tc.want)
		}
	}
}

func TestConvertUnitsTemperature(t *testing.T) {
	res, err := convertUnits(M{"value": 100.0, "from": "c", "to": "f"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := res["value"].(float64); got != 212 {
		t.Fatalf("100C -> %vF, want 212", got)
	}
	res, err = convertUnits(M{"value": 32.0, "from": "f", "to": "c"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := res["value"].(float64); got != 0 {
		t.Fatalf("32F -> %vC, want 0", got)
	}
}

func TestConvertUnitsRejectsIncompatible(t *testing.T) {
	if _, err := convertUnits(M{"value": 1.0, "from": "kg", "to": "km"}); err == nil {
		t.Fatal("kg -> km should fail")
	}
	if _, err := convertUnits(M{"value": 1.0, "from": "c", "to": "kg"}); err == nil {
		t.Fatal("c -> kg should fail")
	}
}

func TestGroundedSearchParsesAnswerAndSources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if !strings.Contains(r.URL.RawQuery, "key=test-key") {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{map[string]any{"text": "Курс доллара 92,5 рубля."}}},
				"groundingMetadata": map[string]any{"groundingChunks": []any{
					map[string]any{"web": map[string]any{"uri": "https://example.com/usd", "title": "USD rate"}},
				}},
			}},
		})
	}))
	defer server.Close()

	oldEndpoint, oldModels, oldKey := geminiSearchEndpoint, geminiSearchModels, key
	geminiSearchEndpoint = server.URL + "/%s?key=%s"
	geminiSearchModels = []string{"models/test-flash"}
	key = "test-key"
	defer func() {
		geminiSearchEndpoint, geminiSearchModels, key = oldEndpoint, oldModels, oldKey
	}()

	res, err := geminiGroundedSearch(context.Background(), "курс доллара")
	if err != nil {
		t.Fatal(err)
	}
	if res["ok"] != true {
		t.Fatalf("result = %+v", res)
	}
	if answer, _ := res["answer"].(string); !strings.Contains(answer, "92,5") {
		t.Fatalf("answer = %q", answer)
	}
	sources, _ := res["sources"].([]M)
	if len(sources) != 1 || sources[0]["uri"] != "https://example.com/usd" {
		t.Fatalf("sources = %+v", sources)
	}
}

func TestSearchAnswerQuotesADifferentSentence(t *testing.T) {
	items := []searchSnippet{{
		Title:   "Курс доллара",
		Snippet: "Курс доллара сегодня вырос до 92,5 рубля. Аналитики связывают это с решением Банка России. На рынке ожидают сохранения ставки до конца квартала.",
		URI:     "https://www.cbr.ru/press/",
	}}
	res := searchResultFromSnippets("bing", "курс доллара", items)
	summary, _ := res["summary"].(string)
	quote, _ := res["quote"].(string)
	answer, _ := res["answer"].(string)
	if summary == "" || quote == "" || answer == "" {
		t.Fatalf("empty answer fields: %+v", res)
	}
	if strings.Contains(summary, quote) || strings.Contains(quote, summary) {
		t.Fatalf("summary and quote overlap: summary=%q quote=%q", summary, quote)
	}
	if strings.Count(answer, quote) != 1 {
		t.Fatalf("quote repeated in answer: %q", answer)
	}
}

func TestSearchAnswerDoesNotQuoteSingleSentenceTwice(t *testing.T) {
	items := []searchSnippet{{
		Title:   "Курс доллара",
		Snippet: "Курс доллара сегодня вырос до 92,5 рубля.",
		URI:     "https://www.cbr.ru/press/",
	}}
	res := searchResultFromSnippets("bing", "курс доллара", items)
	answer, _ := res["answer"].(string)
	if strings.Contains(answer, "Цитирую") {
		t.Fatalf("single sentence must not be quoted again: %q", answer)
	}
	if !strings.Contains(answer, "Источник:") {
		t.Fatalf("source attribution missing: %q", answer)
	}
}

func TestCompactSearchResponseDropsDiagnosticDuplicates(t *testing.T) {
	full := M{
		"ok": true, "summary": "Краткий итог", "quote": "Другая цитата",
		"source_title": "РИА Новости", "answer": "Кратко: итог. Источник: РИА Новости.",
		"sources": []M{{"uri": "https://ria.ru/example"}},
	}
	compact := compactSearchToolResponse(full)
	if _, ok := compact["summary"]; ok {
		t.Fatal("summary leaked to the model")
	}
	if _, ok := compact["quote"]; ok {
		t.Fatal("quote leaked to the model")
	}
	if _, ok := compact["sources"]; ok {
		t.Fatal("source URLs leaked to the model")
	}
	if compact["answer"] != full["answer"] {
		t.Fatalf("answer = %q", compact["answer"])
	}
	if compact["instruction"] == nil {
		t.Fatal("missing single-answer instruction")
	}
}

func TestHandleSearchToolDispatch(t *testing.T) {
	res, ok := handleSearchTool(context.Background(), "convert_units", M{"value": 1.0, "from": "km", "to": "m"})
	if !ok || res["ok"] != true {
		t.Fatalf("convert dispatch = %+v, %v", res, ok)
	}
	if _, ok := handleSearchTool(context.Background(), "get_time", M{}); ok {
		t.Fatal("get_time should not be handled by search tool")
	}
}
