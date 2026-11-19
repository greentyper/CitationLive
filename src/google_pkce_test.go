package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoogleDesktopReleaseClientAndPersonalOverride(t *testing.T) {
	for _, key := range []string{envGoogleClientID, envGoogleClientIDAlt, envGoogleClientSecret, envGoogleSecretAlt, "CITATION_GOOGLE_REDIRECT_URI"} {
		t.Setenv(key, "")
	}
	oldID, oldSecret := releaseGoogleDesktopClientID, releaseGoogleDesktopClientSecret
	releaseGoogleDesktopClientID = "release-client.apps.googleusercontent.com"
	releaseGoogleDesktopClientSecret = "test-desktop-secret"
	t.Cleanup(func() {
		releaseGoogleDesktopClientID, releaseGoogleDesktopClientSecret = oldID, oldSecret
		googlePKCEState.Lock()
		googlePKCEState.session = nil
		googlePKCEState.Unlock()
	})
	dir := t.TempDir()
	cfg, err := LoadGoogleOAuthConfigFrom(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DesktopClient || cfg.ClientID != releaseGoogleDesktopClientID || cfg.ClientSecret != releaseGoogleDesktopClientSecret {
		t.Fatal("release did not select its Desktop OAuth client")
	}
	authURL, err := startGooglePKCEFrom(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if q.Get("redirect_uri") != "http://127.0.0.1:28766" || q.Has("include_granted_scopes") ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		q.Get("access_type") != "offline" || q.Get("state") == "" ||
		!strings.Contains(q.Get("scope"), GoogleScopeTasks) || !strings.Contains(q.Get("scope"), GoogleScopeCalendarEvents) {
		t.Fatal("Desktop authorization URL does not use loopback PKCE and both sync scopes")
	}

	if err := os.WriteFile(filepath.Join(dir, googleClientFileName),
		[]byte(`{"client_id":"personal-client","client_secret":"personal-secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadGoogleOAuthConfigFrom(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DesktopClient || cfg.ClientID != "personal-client" || cfg.ClientSecret != "personal-secret" ||
		googlePKCERedirectURI(cfg) != "http://127.0.0.1:28766/oauth/callback" {
		t.Fatal("personal Web client should take precedence and preserve its callback")
	}
}

func TestGooglePKCEKeepsSessionAfterStateMismatch(t *testing.T) {
	t.Setenv("CITATION_GOOGLE_REDIRECT_URI", "")
	previous := googlePKCEAfterExchange
	googlePKCEAfterExchange = func(string, *GoogleTokenSource) {}
	t.Cleanup(func() { googlePKCEAfterExchange = previous })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(M{
			"access_token": "test-access", "refresh_token": "test-refresh", "expires_in": 3600,
		})
	}))
	defer server.Close()
	cfg := GoogleOAuthConfig{
		ClientID: "test-client", StateDir: t.TempDir(),
		Endpoints: GoogleOAuthEndpoints{Token: server.URL},
	}
	authURL, err := startGooglePKCEFrom(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	state := parsed.Query().Get("state")
	for _, invalid := range []string{"other-state", ""} {
		if _, err := finishGooglePKCEFrom(context.Background(), cfg, "test-code", invalid); err == nil {
			t.Fatal("callback accepted invalid state")
		}
	}
	if _, err := finishGooglePKCEFrom(context.Background(), cfg, "test-code", state); err != nil {
		t.Fatalf("valid callback failed after mismatched state: %v", err)
	}
	if _, err := finishGooglePKCEFrom(context.Background(), cfg, "test-code", state); err == nil {
		t.Fatal("completed session could be used again")
	}
}

func TestGoogleOAuthIncompleteConsentDoesNotReplaceToken(t *testing.T) {
	for _, test := range []struct {
		name, refresh, scope string
	}{
		{name: "no refresh token", scope: GoogleScopeCalendarEvents + " " + GoogleScopeTasks},
		{name: "calendar only", refresh: "test-new-refresh", scope: GoogleScopeCalendarEvents},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(M{
					"access_token": "test-access", "refresh_token": test.refresh, "scope": test.scope,
				})
			}))
			defer server.Close()
			source, err := NewGoogleTokenSource(GoogleOAuthConfig{
				ClientID: "test-client", StateDir: t.TempDir(), Endpoints: GoogleOAuthEndpoints{Token: server.URL},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := source.SaveToken(&GoogleToken{RefreshToken: "test-existing"}); err != nil {
				t.Fatal(err)
			}
			if _, err := source.ExchangeAuthCode(context.Background(), "test-code", "test-verifier", "http://127.0.0.1:28766"); err == nil {
				t.Fatal("incomplete consent was reported as a successful connection")
			}
			tok, err := source.LoadToken()
			if err != nil {
				t.Fatal(err)
			}
			if tok.RefreshToken != "test-existing" {
				t.Fatal("incomplete authorization replaced the stored token")
			}
		})
	}
}

func TestGoogleOAuthRefreshPreservesAccountWhenNoIDTokenReturned(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(M{"access_token": "test-refreshed-access", "expires_in": 3600})
	}))
	defer server.Close()
	cfg := GoogleOAuthConfig{
		ClientID: "test-client", StateDir: t.TempDir(), Endpoints: GoogleOAuthEndpoints{Token: server.URL},
	}
	source, err := NewGoogleTokenSource(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SaveToken(&GoogleToken{RefreshToken: "test-refresh", Email: "test@example.test"}); err != nil {
		t.Fatal(err)
	}
	refreshed, err := source.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Email != "test@example.test" {
		t.Fatal("returned token lost the connected account")
	}
	reloaded, err := NewGoogleTokenSource(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if status := reloaded.Status(); !status.Authorized || status.Email != refreshed.Email {
		t.Fatal("connected account was not persisted after refresh")
	}
}
