package main

import "strings"

func memoryToolDeclarations() []M {
	return []M{
		{
			"name":        "get_personal_context",
			"description": "Return remembered preferences/facts for personal context. Call this before using personal memory, preferences, calendar, reminders, or any personal data. Voice Match is disabled; no extra verification is required.",
			"parameters": M{
				"type": "OBJECT",
				"properties": M{
					"query": M{"type": "STRING", "description": "Optional topic to search in memory."},
				},
			},
		},
		{
			"name":        "remember",
			"description": "Remember an explicit user preference or fact. Do not store passwords, card numbers, PINs, medical data or other secrets.",
			"parameters": M{
				"type": "OBJECT",
				"properties": M{
					"text":     M{"type": "STRING", "description": "The fact/preference to remember, in the user's language."},
					"category": M{"type": "STRING", "description": "Optional category: preference, person, place, routine, fact."},
				},
				"required": []string{"text"},
			},
		},
		{
			"name":        "list_memories",
			"description": "List remembered personal preferences/facts.",
			"parameters":  M{"type": "OBJECT", "properties": M{}},
		},
		{
			"name":        "forget_memory",
			"description": "Forget a remembered preference/fact by id or text.",
			"parameters": M{
				"type": "OBJECT",
				"properties": M{
					"id":   M{"type": "STRING", "description": "Memory id from list_memories."},
					"text": M{"type": "STRING", "description": "Text to match and forget."},
				},
			},
		},
	}
}

func voiceNotRecognized() M {
	return M{
		"ok":          false,
		"voice_match": false,
		"error":       "voice not recognized",
		"instruction": "Скажи по-русски: «Я не распознал ваш голос, поэтому не могу получить доступ к личным данным.» Не раскрывай, есть ли сохранённая память.",
	}
}

func memoryLooksSecret(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range []string{
		"пароль", "password", "passcode", "пин-код", "pin code", "cvv", "cvc",
		"номер карты", "card number", "секретный код", "seed phrase", "приватный ключ",
		"private key", "паспорт", "passport number", "снилс",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func handleMemoryTool(name string, args M) (M, bool) {
	switch name {
	case "get_personal_context":
		if !personalAccessOK() {
			res := personalAccessRequired()
			res["memory_available"] = false
			res["memories"] = []M{}
			return res, true
		}
		query, _ := args["query"].(string)
		var entries []MemoryEntry
		if strings.TrimSpace(query) == "" {
			entries = personalMemory.List()
			if len(entries) > 12 {
				entries = entries[:12]
			}
		} else {
			entries = personalMemory.Search(query, 8)
		}
		return M{
			"ok":               true,
			"voice_match":      true,
			"memory_available": true,
			"memories":         memoryEntriesJSON(entries),
			"instruction":      "Используй эти факты естественно, только если они относятся к вопросу. Не упоминай хранилище памяти. Если пользователь просит забыть что-то — вызови forget_memory.",
		}, true
	case "remember":
		if !personalAccessOK() {
			return personalAccessRequired(), true
		}
		text, _ := args["text"].(string)
		category, _ := args["category"].(string)
		text = strings.TrimSpace(text)
		if text == "" {
			return M{"ok": false, "error": "memory text is required"}, true
		}
		if memoryLooksSecret(text) {
			return M{"ok": false, "error": "refusing to store a secret", "instruction": "Вежливо скажи, что пароли, карты и секретные данные не сохраняются."}, true
		}
		entry, err := personalMemory.Add(text, category)
		if err != nil {
			return M{"ok": false, "error": clean(err)}, true
		}
		return M{
			"ok": true, "id": entry.ID, "text": entry.Text, "category": entry.Category,
			"instruction": "Коротко подтверди по-русски, что запомнил.",
		}, true
	case "list_memories":
		if !personalAccessOK() {
			return personalAccessRequired(), true
		}
		return M{"ok": true, "memories": memoryEntriesJSON(personalMemory.List())}, true
	case "forget_memory":
		if !personalAccessOK() {
			return personalAccessRequired(), true
		}
		id, _ := args["id"].(string)
		text, _ := args["text"].(string)
		deleted, err := personalMemory.Delete(id, text)
		if err != nil {
			return M{"ok": false, "error": clean(err)}, true
		}
		return M{"ok": true, "deleted": deleted, "instruction": "Коротко подтверди по-русски, что забыл."}, true
	default:
		return nil, false
	}
}
