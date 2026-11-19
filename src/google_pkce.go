package main

// Google OAuth 2.0 authorization-code + PKCE for Calendar/Tasks.
//
// Google's device flow cannot request Calendar/Tasks scopes for this device
// type, so the speaker uses the installed-app loopback flow. The user opens
// the authorization URL in a browser; Google redirects to the loopback URI
// (normally http://127.0.0.1:28766/oauth/callback, forwarded to the speaker).
// The one-time code is exchanged with the PKCE verifier and the refresh token
// is persisted in state/google_oauth.json.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const googlePKCESessionTTL = 10 * time.Minute

var googlePKCEAuthEndpoint = "https://accounts.google.com/o/oauth2/v2/auth"

// googlePKCEAfterExchange is replaceable in tests to avoid network sync.
var googlePKCEAfterExchange = installGoogleSyncBackend

type googlePKCESession struct {
	Verifier    string
	State       string
	RedirectURI string
	ClientID    string
	Created     time.Time
}

var googlePKCEState struct {
	sync.Mutex
	session *googlePKCESession
}

func googlePKCERedirectURI(cfg GoogleOAuthConfig) string {
	if cfg.DesktopClient {
		return "http://127.0.0.1:28766"
	}
	if v := strings.TrimSpace(os.Getenv("CITATION_GOOGLE_REDIRECT_URI")); v != "" {
		return v
	}
	return "http://127.0.0.1:28766/oauth/callback"
}

func randomPKCEToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// startGooglePKCEFrom creates a PKCE session and returns the authorization URL.
func startGooglePKCEFrom(cfg GoogleOAuthConfig) (string, error) {
	normalized, err := cfg.normalized()
	if err != nil {
		return "", err
	}
	verifier, err := randomPKCEToken(32)
	if err != nil {
		return "", err
	}
	state, err := randomPKCEToken(16)
	if err != nil {
		return "", err
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])
	redirectURI := googlePKCERedirectURI(normalized)
	googlePKCEState.Lock()
	googlePKCEState.session = &googlePKCESession{
		Verifier:    verifier,
		State:       state,
		RedirectURI: redirectURI,
		ClientID:    normalized.ClientID,
		Created:     time.Now(),
	}
	googlePKCEState.Unlock()

	q := url.Values{}
	q.Set("client_id", normalized.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(normalized.Scopes, " "))
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	if !normalized.DesktopClient {
		q.Set("include_granted_scopes", "true")
	}
	q.Set("state", state)
	return googlePKCEAuthEndpoint + "?" + q.Encode(), nil
}

// startGooglePKCE uses the environment/file client configuration.
func startGooglePKCE() (string, string, error) {
	cfg, err := LoadGoogleOAuthConfig()
	if err != nil {
		return "", "", err
	}
	authURL, err := startGooglePKCEFrom(cfg)
	return authURL, googlePKCERedirectURI(cfg), err
}

// finishGooglePKCEFrom validates the state and exchanges the code.
func finishGooglePKCEFrom(ctx context.Context, cfg GoogleOAuthConfig, code, state string) (*GoogleToken, error) {
	if strings.TrimSpace(code) == "" {
		return nil, errors.New("google oauth: authorization code is missing")
	}
	googlePKCEState.Lock()
	session := googlePKCEState.session
	if session == nil {
		googlePKCEState.Unlock()
		return nil, errors.New("google oauth: no pending authorization session")
	}
	if time.Since(session.Created) > googlePKCESessionTTL {
		googlePKCEState.session = nil
		googlePKCEState.Unlock()
		return nil, errors.New("google oauth: authorization session expired")
	}
	if state == "" || state != session.State {
		googlePKCEState.Unlock()
		return nil, errors.New("google oauth: state mismatch")
	}
	if cfg.ClientID != session.ClientID {
		googlePKCEState.Unlock()
		return nil, errors.New("google oauth: client changed during authorization")
	}
	googlePKCEState.session = nil
	googlePKCEState.Unlock()
	source, err := NewGoogleTokenSource(cfg)
	if err != nil {
		return nil, err
	}
	tok, err := source.ExchangeAuthCode(ctx, code, session.Verifier, session.RedirectURI)
	if err != nil {
		return nil, err
	}
	googlePKCEAfterExchange(cfg.StateDir, source)
	return tok, nil
}

func finishGooglePKCE(ctx context.Context, code, state string) (*GoogleToken, error) {
	cfg, err := LoadGoogleOAuthConfig()
	if err != nil {
		return nil, err
	}
	return finishGooglePKCEFrom(ctx, cfg, code, state)
}

var googleSync struct {
	sync.RWMutex
	source *GoogleTokenSource
	sync   *CalendarSync
}

func installGoogleSyncBackend(dir string, source *GoogleTokenSource) {
	if dir == "" {
		dir = stateDirectory
	}
	cs := NewCalendarSync(dir, source)
	googleSync.Lock()
	googleSync.source = source
	googleSync.sync = cs
	googleSync.Unlock()
	setReminderBackend(cs)
}

func googleSyncBackend() (*GoogleTokenSource, *CalendarSync) {
	googleSync.RLock()
	defer googleSync.RUnlock()
	return googleSync.source, googleSync.sync
}

type googleOAuthStatus struct {
	Configured bool      `json:"configured"`
	Authorized bool      `json:"authorized"`
	Email      string    `json:"email,omitempty"`
	Expiry     time.Time `json:"expiry,omitempty"`
	Scopes     []string  `json:"scopes,omitempty"`
	Error      string    `json:"error,omitempty"`
}

func currentGoogleOAuthStatus() googleOAuthStatus {
	cfg, err := LoadGoogleOAuthConfig()
	if err != nil {
		return googleOAuthStatus{Error: clean(err)}
	}
	source, err := NewGoogleTokenSource(cfg)
	if err != nil {
		return googleOAuthStatus{Configured: true, Error: clean(err)}
	}
	st := source.Status()
	return googleOAuthStatus{
		Configured: true,
		Authorized: st.Authorized,
		Email:      st.Email,
		Expiry:     st.Expiry,
		Scopes:     st.Scopes,
	}
}
