package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

const notesRemoteTimeout = 10 * time.Second

func noteToolDeclarations() []M {
	return []M{
		{
			"name":        "create_note",
			"description": "Создать заметку или чек-лист. При авторизации Google сохраняет в Google Tasks; иначе использует локальный кэш и работает офлайн. Не сохраняй пароли, номера карт, PIN и другие секреты.",
			"parameters": M{
				"type": "OBJECT",
				"properties": M{
					"title": M{"type": "STRING", "description": "Название заметки, коротко, на языке пользователя."},
					"body":  M{"type": "STRING", "description": "Необязательный текст заметки."},
				},
				"required": []string{"title"},
			},
		},
		{
			"name":        "add_list_item",
			"description": "Добавить пункт в заметку или чек-лист. При авторизации Google обновляет Google Tasks; иначе сохраняет локально. Укажи note_id из list_notes или title заметки, а также text пункта. Не сохраняй секреты.",
			"parameters": M{
				"type": "OBJECT",
				"properties": M{
					"note_id": M{"type": "STRING", "description": "Идентификатор заметки, например n1 или g:task-id."},
					"title":   M{"type": "STRING", "description": "Название заметки, если нет note_id."},
					"text":    M{"type": "STRING", "description": "Текст нового пункта списка."},
				},
				"required": []string{"text"},
			},
		},
		{
			"name":        "read_note",
			"description": "Прочитать заметку владельца по id или title из Google Tasks или локального кэша.",
			"parameters": M{
				"type": "OBJECT",
				"properties": M{
					"id":    M{"type": "STRING", "description": "Идентификатор заметки, например n1 или g:task-id."},
					"title": M{"type": "STRING", "description": "Название заметки, если нет id."},
				},
			},
		},
		{
			"name":        "list_notes",
			"description": "Показать список заметок владельца: id, title, item_count, open_items, updated. При авторизации Google читает Google Tasks и объединяет с локальным кэшем; иначе показывает локальный кэш.",
			"parameters":  M{"type": "OBJECT", "properties": M{}},
		},
		{
			"name":        "complete_list_item",
			"description": "Отметить пункт заметки выполненным. При авторизации Google обновляет Google Tasks; иначе сохраняет локально. Укажи note_id или title, затем text пункта или index (нумерация с 1).",
			"parameters": M{
				"type": "OBJECT",
				"properties": M{
					"note_id": M{"type": "STRING", "description": "Идентификатор заметки, например n1 или g:task-id."},
					"title":   M{"type": "STRING", "description": "Название заметки, если нет note_id."},
					"text":    M{"type": "STRING", "description": "Текст пункта, который нужно отметить выполненным."},
					"index":   M{"type": "INTEGER", "description": "Номер пункта с 1, если text не указан."},
				},
			},
		},
		{
			"name":        "delete_note",
			"description": "Удалить заметку владельца по id или title. При авторизации Google удаляет задачу из Google Tasks; иначе удаляет локальную копию.",
			"parameters": M{
				"type": "OBJECT",
				"properties": M{
					"id":    M{"type": "STRING", "description": "Идентификатор заметки, например n1 или g:task-id."},
					"title": M{"type": "STRING", "description": "Название заметки, если нет id."},
				},
			},
		},
	}
}

func noteSecretRefusal() M {
	return M{
		"ok":          false,
		"error":       "refusing to store a secret",
		"instruction": "Вежливо скажи по-русски, что пароли, номера карт, PIN и другие секретные данные в заметках не сохраняются.",
	}
}

func noteTarget(args M) (id, title string) {
	id, _ = args["note_id"].(string)
	if strings.TrimSpace(id) == "" {
		id, _ = args["id"].(string)
	}
	title, _ = args["title"].(string)
	return strings.TrimSpace(id), strings.TrimSpace(title)
}

func noteIndexArg(args M) int {
	switch value := args["index"].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	case string:
		number, err := strconv.Atoi(strings.TrimSpace(value))
		if err == nil {
			return number
		}
	}
	return 0
}

// notesGoogleBackend returns the shared Google Tasks backend when OAuth has
// installed one. A nil backend means local-only operation.
func notesGoogleBackend() (*CalendarSync, string, bool) {
	_, cs := googleSyncBackend()
	if cs == nil || cs.Tasks == nil {
		return nil, "", false
	}
	list := strings.TrimSpace(cs.TaskList)
	if list == "" {
		list = "@default"
	}
	return cs, list, true
}

func notesRemoteContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), notesRemoteTimeout)
}

func noteLocalFallback(res M, err error) M {
	res["source"] = "local"
	res["offline"] = true
	res["pending_sync"] = true
	if err != nil {
		res["sync_error"] = clean(err)
	}
	return res
}

func noteGoogleSuccess(res M, note Note) M {
	res["note"] = noteJSON(note)
	res["remote_id"] = note.RemoteID
	res["source"] = "google_tasks"
	res["offline"] = false
	res["pending_sync"] = false
	return res
}

// syncNoteMutation pushes the current note to Google Tasks. A note without a
// remote id is created; otherwise the existing task is updated. Any failure
// leaves the local note intact and reports the offline/pending state.
func syncNoteMutation(res M, note Note, instruction string) M {
	res["instruction"] = instruction
	cs, list, ok := notesGoogleBackend()
	if !ok {
		return noteLocalFallback(res, nil)
	}
	ctx, cancel := notesRemoteContext()
	defer cancel()
	if strings.TrimSpace(note.RemoteID) == "" {
		created, err := cs.Tasks.CreateTask(ctx, list, GoogleTask{
			Title:  note.Title,
			Notes:  noteGoogleBody(note),
			Status: "needsAction",
		})
		if err != nil {
			return noteLocalFallback(res, err)
		}
		if strings.TrimSpace(created.ID) == "" {
			return noteLocalFallback(res, errors.New("google task id is missing"))
		}
		updated, err := localNotes.SetRemote(note.ID, created.ID, list)
		if err != nil {
			return noteLocalFallback(res, err)
		}
		note = updated
	} else {
		if _, err := cs.Tasks.UpdateTask(ctx, list, GoogleTask{
			ID:     note.RemoteID,
			Title:  note.Title,
			Notes:  noteGoogleBody(note),
			Status: "needsAction",
		}); err != nil {
			return noteLocalFallback(res, err)
		}
	}
	return noteGoogleSuccess(res, note)
}

func handleNoteTool(name string, args M) (M, bool) {
	switch name {
	case "create_note":
		if !personalAccessOK() {
			return personalAccessRequired(), true
		}
		title, _ := args["title"].(string)
		body, _ := args["body"].(string)
		title = strings.TrimSpace(title)
		body = strings.TrimSpace(body)
		if title == "" {
			return M{"ok": false, "error": "note title is required", "instruction": "Спроси по-русски, как назвать заметку."}, true
		}
		if memoryLooksSecret(title) || memoryLooksSecret(body) {
			return noteSecretRefusal(), true
		}
		note, err := localNotes.Create(title, body)
		if err != nil {
			return M{"ok": false, "error": clean(err)}, true
		}
		res := M{"ok": true, "id": note.ID, "title": note.Title, "note": noteJSON(note)}
		return syncNoteMutation(res, note, "Коротко подтверди по-русски, что заметка создана."), true
	case "add_list_item":
		if !personalAccessOK() {
			return personalAccessRequired(), true
		}
		id, title := noteTarget(args)
		text, _ := args["text"].(string)
		text = strings.TrimSpace(text)
		if id == "" && title == "" {
			return M{"ok": false, "error": "note id or title is required", "instruction": "Спроси по-русски, в какую заметку добавить пункт."}, true
		}
		if text == "" {
			return M{"ok": false, "error": "list item text is required", "instruction": "Спроси по-русски, что добавить в список."}, true
		}
		if memoryLooksSecret(title) || memoryLooksSecret(text) {
			return noteSecretRefusal(), true
		}
		note, index, err := localNotes.AddItem(id, title, text)
		if err != nil {
			return M{"ok": false, "error": clean(err)}, true
		}
		res := M{
			"ok": true, "note_id": note.ID, "title": note.Title, "index": index,
			"text": text, "note": noteJSON(note),
		}
		return syncNoteMutation(res, note, "Коротко подтверди по-русски, что пункт добавлен."), true
	case "read_note":
		if !personalAccessOK() {
			return personalAccessRequired(), true
		}
		id, title := noteTarget(args)
		if id == "" && title == "" {
			return M{"ok": false, "error": "note id or title is required", "instruction": "Спроси по-русски, какую заметку прочитать."}, true
		}
		note, err := localNotes.Get(id, title)
		if err != nil {
			return M{"ok": false, "error": clean(err)}, true
		}
		return M{
			"ok": true, "note": noteJSON(note), "source": "local",
			"instruction": "Перескажи заметку по-русски, сохраняя текст пунктов. Если есть незавершённые пункты, можешь кратко их перечислить.",
		}, true
	case "list_notes":
		if !personalAccessOK() {
			return personalAccessRequired(), true
		}
		notes := localNotes.List()
		source := "local"
		offline := true
		pendingSync := true
		var syncErr error
		if cs, list, ok := notesGoogleBackend(); ok {
			ctx, cancel := notesRemoteContext()
			tasks, err := cs.Tasks.ListTasks(ctx, list)
			cancel()
			if err != nil {
				syncErr = err
			} else {
				merged, mergeErr := localNotes.MergeRemote(tasks, list)
				if mergeErr != nil {
					syncErr = mergeErr
				}
				notes = merged
				source = "google_tasks"
				offline = false
				pendingSync = mergeErr != nil
			}
		}
		out := make([]M, 0, len(notes))
		for _, note := range notes {
			out = append(out, noteSummaryJSON(note))
		}
		res := M{
			"ok": true, "notes": out, "source": source,
			"offline": offline, "pending_sync": pendingSync,
			"instruction": "Перечисли заметки на языке пользователя. Если заметок нет, скажи, что список пуст.",
		}
		if syncErr != nil {
			res["sync_error"] = clean(syncErr)
		}
		return res, true
	case "complete_list_item":
		if !personalAccessOK() {
			return personalAccessRequired(), true
		}
		id, title := noteTarget(args)
		text, _ := args["text"].(string)
		text = strings.TrimSpace(text)
		index := noteIndexArg(args)
		if id == "" && title == "" {
			return M{"ok": false, "error": "note id or title is required", "instruction": "Спроси по-русски, в какой заметке отметить пункт."}, true
		}
		if text == "" && index <= 0 {
			return M{"ok": false, "error": "list item text or index is required", "instruction": "Спроси по-русски, какой пункт выполнен."}, true
		}
		note, itemIndex, alreadyDone, err := localNotes.CompleteItem(id, title, text, index)
		if err != nil {
			return M{"ok": false, "error": clean(err)}, true
		}
		res := M{
			"ok": true, "note_id": note.ID, "title": note.Title, "index": itemIndex,
			"done": true, "already_done": alreadyDone, "note": noteJSON(note),
		}
		return syncNoteMutation(res, note, "Коротко подтверди по-русски, что пункт отмечен выполненным."), true
	case "delete_note":
		if !personalAccessOK() {
			return personalAccessRequired(), true
		}
		id, title := noteTarget(args)
		if id == "" && title == "" {
			return M{"ok": false, "error": "note id or title is required", "instruction": "Спроси по-русски, какую заметку удалить."}, true
		}
		note, err := localNotes.Get(id, title)
		if err != nil {
			return M{"ok": false, "error": clean(err)}, true
		}
		cs, list, googleOK := notesGoogleBackend()
		remoteDeleted := false
		if googleOK && strings.TrimSpace(note.RemoteID) != "" {
			ctx, cancel := notesRemoteContext()
			err := cs.Tasks.DeleteTask(ctx, list, note.RemoteID)
			cancel()
			if err != nil {
				res := M{
					"ok": false, "deleted": false, "error": clean(err),
					"id": note.ID, "title": note.Title,
					"instruction": "Скажи по-русски, что не удалось удалить заметку в Google Tasks, локальная копия сохранена.",
				}
				return noteLocalFallback(res, err), true
			}
			remoteDeleted = true
		}
		deleted, err := localNotes.Delete(note.ID, "")
		if err != nil {
			return M{"ok": false, "error": clean(err)}, true
		}
		res := M{
			"ok": true, "deleted": true, "id": deleted.ID, "title": deleted.Title,
			"instruction": "Коротко подтверди по-русски, что заметка удалена.",
		}
		switch {
		case remoteDeleted:
			res["source"] = "google_tasks"
			res["offline"] = false
			res["pending_sync"] = false
		case !googleOK && strings.TrimSpace(note.RemoteID) != "":
			noteLocalFallback(res, nil)
		default:
			res["source"] = "local"
			res["offline"] = false
			res["pending_sync"] = false
		}
		return res, true
	default:
		return nil, false
	}
}
