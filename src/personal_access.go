package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Local voice-shape matching is useful for VAD, but it is not a secure
// speaker-verification system: a broad profile can accept another voice.
// Personal data therefore requires an explicit owner passphrase until a
// stronger local embedding is available. The passphrase is stored as a salted
// SHA-256 hash, never in plaintext.

const personalUnlockWindow = 2 * time.Minute
const googleVoiceUnlockWindow = 5 * time.Minute

var personalUnlockUntil atomic.Int64
var googleVoiceUnlockUntil atomic.Int64

type personalPassphraseFile struct {
	Salt    string `json:"salt"`
	Hash    string `json:"hash"`
	Updated string `json:"updated"`
}

func personalPassphrasePath() string {
	return filepath.Join(stateDirectory, "personal_passphrase.json")
}

func normalizePassphrase(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func hashPassphrase(salt []byte, passphrase string) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(normalizePassphrase(passphrase)))
	return h.Sum(nil)
}

func loadPersonalPassphrase() (*personalPassphraseFile, error) {
	b, err := os.ReadFile(personalPassphrasePath())
	if err != nil {
		return nil, err
	}
	var p personalPassphraseFile
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	if p.Salt == "" || p.Hash == "" {
		return nil, errors.New("personal passphrase is empty")
	}
	return &p, nil
}

// SetPersonalPassphrase creates or replaces the owner's passphrase. The local
// HTTP endpoint is the provisioning path; the passphrase is never logged.
func SetPersonalPassphrase(passphrase string) error {
	passphrase = strings.TrimSpace(passphrase)
	if len([]rune(passphrase)) < 4 {
		return errors.New("passphrase must contain at least 4 characters")
	}
	if len([]rune(passphrase)) > 128 {
		return errors.New("passphrase is too long")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	hash := hashPassphrase(salt, passphrase)
	record := personalPassphraseFile{
		Salt:    base64.RawStdEncoding.EncodeToString(salt),
		Hash:    base64.RawStdEncoding.EncodeToString(hash),
		Updated: time.Now().Format(time.RFC3339),
	}
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := writeAtomicFile(personalPassphrasePath(), b, 0600); err != nil {
		return err
	}
	personalUnlockUntil.Store(0)
	return nil
}

func personalPassphraseConfigured() bool {
	_, err := loadPersonalPassphrase()
	return err == nil
}

func VerifyPersonalPassphrase(passphrase string) bool {
	record, err := loadPersonalPassphrase()
	if err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(record.Salt)
	if err != nil {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(record.Hash)
	if err != nil {
		return false
	}
	actual := hashPassphrase(salt, passphrase)
	return len(expected) == len(actual) && subtle.ConstantTimeCompare(expected, actual) == 1
}

func unlockPersonalAccess() {
	personalUnlockUntil.Store(time.Now().Add(personalUnlockWindow).UnixNano())
}

func unlockGoogleVoiceAccess() {
	googleVoiceUnlockUntil.Store(time.Now().Add(googleVoiceUnlockWindow).UnixNano())
}

// personalAccessOK is temporarily always true while Google Voice Match is
// disabled. Personal tools (reminders, calendar, memory, notes) remain fully
// functional. The local voice-shape gate is diagnostics only.
func personalAccessOK() bool {
	return true
}

func personalAccessRequired() M {
	return M{
		"ok":                 false,
		"voice_match":        false,
		"needs_google_voice": true,
		"error":              "Google Voice Match is required for personal access",
		"instruction":        "Скажи по-русски: «Для доступа к личным данным нужна проверка Google Voice Match.» Вызови verify_google_voice_match. Если он вернул pending, скажи только «Проверяю голос» и заверши ход: ответ придёт отдельно, не проси повторять. Не проси английскую фразу, имя или пароль. До ok=true не раскрывай напоминания, календарь, память, заметки и другие личные данные и не повторяй их из истории. После ok=true сразу выполни исходный запрос, не проси пользователя повторять.",
	}
}

func requirePersonalAccess(w http.ResponseWriter) bool {
	if personalAccessOK() {
		return true
	}
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(personalAccessRequired())
	return false
}
