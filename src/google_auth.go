package main

// Google OAuth 2.0 device authorization (RFC 8628) for Calendar/Tasks sync.
//
// This file intentionally does not depend on any Google client library: the
// speaker ships a single static ARMv7 binary and the device flow is small
// enough to implement directly. It does not call any stock Assistant RPCs;
// it only obtains and refreshes an OAuth token for the Calendar and Tasks
// REST APIs.
//
// Secrets (access token, refresh token, client secret, id_token) are never
// written to logs or embedded in error strings. Token material is persisted
// only to <state>/google_oauth.json with mode 0600 using the atomic writer
// from assistant_info.go.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	googleDeviceCodeEndpoint    = "https://oauth2.googleapis.com/device/code"
	googleTokenEndpoint         = "https://oauth2.googleapis.com/token"
	googleDeviceVerificationURL = "https://www.google.com/device"

	googleTokenFileName  = "google_oauth.json"
	googleClientFileName = "google_oauth_client.json"

	googleTokenFileVersion = 1

	googleTokenRefreshSkew     = 90 * time.Second
	googleDefaultHTTPTimeout   = 20 * time.Second
	googleDefaultPollInterval  = 5 * time.Second
	googleDefaultDeviceExpiry  = 30 * time.Minute
	googleAuthMaxResponseBytes = 1 << 20
)

// Google OAuth scopes required for Calendar/Tasks sync. openid/email are only
// used to display the authorized account and are optional.
const (
	GoogleScopeCalendarEvents = "https://www.googleapis.com/auth/calendar.events"
	GoogleScopeTasks          = "https://www.googleapis.com/auth/tasks"
	GoogleScopeOpenID         = "openid"
	GoogleScopeEmail          = "email"
)

// Environment variables consulted by LoadGoogleOAuthConfig. The CITATION_
// names take precedence over the generic GOOGLE_OAUTH_ names.
const (
	envGoogleClientID     = "CITATION_GOOGLE_CLIENT_ID"
	envGoogleClientSecret = "CITATION_GOOGLE_CLIENT_SECRET"
	envGoogleClientIDAlt  = "GOOGLE_OAUTH_CLIENT_ID"
	envGoogleSecretAlt    = "GOOGLE_OAUTH_CLIENT_SECRET"
)

// Sentinel errors returned by the device flow. Callers can use errors.Is.
var (
	ErrGoogleAuthorizationPending = errors.New("google oauth: authorization pending")
	ErrGoogleSlowDown             = errors.New("google oauth: slow down")
	ErrGoogleAccessDenied         = errors.New("google oauth: access denied")
	ErrGoogleExpiredToken         = errors.New("google oauth: device code expired")
	ErrGoogleNotAuthorized        = errors.New("google oauth: not authorized")
	ErrGoogleRefreshRejected      = errors.New("google oauth: refresh token rejected")
	ErrGoogleInvalidClient        = errors.New("google oauth: invalid client")
)

// GoogleRequiredScopes returns the scopes that Calendar/Tasks sync cannot work
// without. They are always requested, even if a caller supplies a narrower
// scope list.
func GoogleRequiredScopes() []string {
	return []string{GoogleScopeCalendarEvents, GoogleScopeTasks}
}

// GoogleSyncScopes returns the default scope set: required sync scopes plus
// openid/email for account display.
func GoogleSyncScopes() []string {
	return []string{GoogleScopeCalendarEvents, GoogleScopeTasks, GoogleScopeOpenID, GoogleScopeEmail}
}

// GoogleOAuthEndpoints lets tests (or a future proxy) override the two
// endpoints. Empty fields fall back to Google's public endpoints.
type GoogleOAuthEndpoints struct {
	DeviceCode string
	Token      string
}

// GoogleOAuthConfig configures a GoogleTokenSource. ClientID is required.
// ClientSecret is optional: Google's "TVs and Limited Input devices" client
// type does not use one, but a confidential client may.
type GoogleOAuthConfig struct {
	ClientID     string
	ClientSecret string
	Scopes       []string
	StateDir     string // defaults to stateDirectory
	Endpoints    GoogleOAuthEndpoints
	HTTPClient   *http.Client
	Now          func() time.Time
}

// DeviceAuthSession is the result of StartDeviceAuth. DeviceCode is secret
// (it is the polling credential) and is never logged; UserCode is meant to be
// shown to the user.
type DeviceAuthSession struct {
	DeviceCode              string
	UserCode                string
	VerificationURL         string
	VerificationURLComplete string
	Interval                time.Duration
	ExpiresIn               time.Duration
	ExpiresAt               time.Time
}

// String redacts the device code so a session can be logged safely.
func (s DeviceAuthSession) String() string {
	return fmt.Sprintf("DeviceAuthSession{user_code:%s verification_url:%s interval:%s expires_in:%s device_code:[redacted]}",
		s.UserCode, s.VerificationURL, s.Interval, s.ExpiresIn)
}

// GoString makes %#v safe as well.
func (s DeviceAuthSession) GoString() string { return s.String() }

// Instruction returns a ready-to-speak/display authorization instruction.
func (s DeviceAuthSession) Instruction() string {
	u := s.VerificationURL
	if u == "" {
		u = googleDeviceVerificationURL
	}
	return fmt.Sprintf("Open %s and enter code %s", u, s.UserCode)
}

// GoogleToken is an OAuth token plus the metadata needed for sync. Token
// values are excluded from String()/GoString() so accidental logging cannot
// leak them.
type GoogleToken struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	Expiry       time.Time
	Scopes       []string
	Email        string
	IDToken      string
}

// String redacts all credential material.
func (t GoogleToken) String() string {
	return fmt.Sprintf("GoogleToken{token_type:%s expiry:%s scopes:%v email:%s access_token:[redacted] refresh_token:[redacted] id_token:[redacted]}",
		t.TokenType, t.Expiry.UTC().Format(time.RFC3339), t.Scopes, t.Email)
}

// GoString makes %#v safe as well.
func (t GoogleToken) GoString() string { return t.String() }

func (t GoogleToken) clone() *GoogleToken {
	c := t
	c.Scopes = append([]string(nil), t.Scopes...)
	return &c
}

// GoogleAuthStatus is a redacted view of the stored authorization, suitable
// for a settings screen or status endpoint.
type GoogleAuthStatus struct {
	Authorized bool
	Email      string
	Expiry     time.Time
	Scopes     []string
}

// TokenProvider is the credential source consumed by calendar_sync.go.
// GoogleTokenSource implements the shared TokenProvider interface declared in
// calendar_models.go.
var _ TokenProvider = (*GoogleTokenSource)(nil)

// GoogleTokenSource owns the device flow, the on-disk token, and refresh.
// It is safe for concurrent use.
type GoogleTokenSource struct {
	cfg GoogleOAuthConfig

	mu      sync.Mutex
	token   *GoogleToken
	loaded  bool
	loadErr error

	refreshMu sync.Mutex

	// pollInterval overrides the server-advertised interval. Tests set it to
	// keep AuthorizeDevice fast; production leaves it zero.
	pollInterval time.Duration
}

// NewGoogleTokenSource validates and normalizes cfg.
func NewGoogleTokenSource(cfg GoogleOAuthConfig) (*GoogleTokenSource, error) {
	normalized, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	return &GoogleTokenSource{cfg: normalized}, nil
}

// NewGoogleTokenSourceFromEnv builds a source from environment variables or
// <state>/google_oauth_client.json. See LoadGoogleOAuthConfig.
func NewGoogleTokenSourceFromEnv() (*GoogleTokenSource, error) {
	cfg, err := LoadGoogleOAuthConfig()
	if err != nil {
		return nil, err
	}
	return NewGoogleTokenSource(cfg)
}

func (c GoogleOAuthConfig) normalized() (GoogleOAuthConfig, error) {
	if strings.TrimSpace(c.ClientID) == "" {
		return c, errors.New("google oauth: client id is not configured")
	}
	if c.StateDir == "" {
		c.StateDir = stateDirectory
	}
	c.Scopes = ensureGoogleScopes(c.Scopes)
	if c.Endpoints.DeviceCode == "" {
		c.Endpoints.DeviceCode = googleDeviceCodeEndpoint
	}
	if c.Endpoints.Token == "" {
		c.Endpoints.Token = googleTokenEndpoint
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	for _, ep := range []string{c.Endpoints.DeviceCode, c.Endpoints.Token} {
		if err := validateGoogleEndpoint(ep); err != nil {
			return c, err
		}
	}
	return c, nil
}

func ensureGoogleScopes(in []string) []string {
	out := make([]string, 0, len(in)+len(GoogleRequiredScopes()))
	seen := make(map[string]bool, len(in)+2)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, s := range in {
		add(s)
	}
	for _, s := range GoogleRequiredScopes() {
		add(s)
	}
	return out
}

// validateGoogleEndpoint requires HTTPS, except for loopback endpoints which
// exist only so httptest can exercise the flow.
func validateGoogleEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("google oauth: invalid endpoint url")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
	}
	return errors.New("google oauth: endpoint must use https")
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// LoadGoogleOAuthConfig reads client credentials from the environment first,
// then from <state>/google_oauth_client.json. Client IDs are not secrets, but
// the file may contain a client secret, so it is written/read as 0600.
func LoadGoogleOAuthConfig() (GoogleOAuthConfig, error) {
	return LoadGoogleOAuthConfigFrom(stateDirectory)
}

// LoadGoogleOAuthConfigFrom is LoadGoogleOAuthConfig with an explicit state
// directory (used by tests).
func LoadGoogleOAuthConfigFrom(dir string) (GoogleOAuthConfig, error) {
	cfg := GoogleOAuthConfig{StateDir: dir, Scopes: GoogleSyncScopes()}
	if dir == "" {
		cfg.StateDir = stateDirectory
	}

	path := filepath.Join(cfg.StateDir, googleClientFileName)
	if b, err := os.ReadFile(path); err == nil {
		var stored struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		}
		if err := json.Unmarshal(b, &stored); err != nil {
			return cfg, errors.New("google oauth: client config is not valid JSON")
		}
		cfg.ClientID = strings.TrimSpace(stored.ClientID)
		cfg.ClientSecret = strings.TrimSpace(stored.ClientSecret)
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0077 != 0 {
			_ = os.Chmod(path, 0600)
		}
	}

	if v := firstNonEmptyEnv(envGoogleClientID, envGoogleClientIDAlt); v != "" {
		cfg.ClientID = v
	}
	if v := firstNonEmptyEnv(envGoogleClientSecret, envGoogleSecretAlt); v != "" {
		cfg.ClientSecret = v
	}
	if cfg.ClientID == "" {
		return cfg, errors.New("google oauth: client id is not configured (set " + envGoogleClientID + " or write " + path + ")")
	}
	return cfg, nil
}

func firstNonEmptyEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

func (g *GoogleTokenSource) stateDir() string {
	if g.cfg.StateDir != "" {
		return g.cfg.StateDir
	}
	return stateDirectory
}

// TokenPath is where the token is persisted.
func (g *GoogleTokenSource) TokenPath() string {
	return filepath.Join(g.stateDir(), googleTokenFileName)
}

func (g *GoogleTokenSource) httpClient() *http.Client {
	if g.cfg.HTTPClient != nil {
		return g.cfg.HTTPClient
	}
	return &http.Client{Timeout: googleDefaultHTTPTimeout}
}

func (g *GoogleTokenSource) now() time.Time {
	if g.cfg.Now != nil {
		return g.cfg.Now()
	}
	return time.Now()
}

// StartDeviceAuth requests a device code (RFC 8628 section 3.1/3.2).
func (g *GoogleTokenSource) StartDeviceAuth(ctx context.Context) (*DeviceAuthSession, error) {
	form := url.Values{}
	form.Set("client_id", g.cfg.ClientID)
	form.Set("scope", strings.Join(g.cfg.Scopes, " "))
	if g.cfg.ClientSecret != "" {
		form.Set("client_secret", g.cfg.ClientSecret)
	}

	var resp struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURL         string `json:"verification_url"`
		VerificationURLComplete string `json:"verification_url_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		Interval                int64  `json:"interval"`
	}
	if err := g.postForm(ctx, g.cfg.Endpoints.DeviceCode, form, &resp); err != nil {
		return nil, err
	}
	if resp.DeviceCode == "" || resp.UserCode == "" {
		return nil, errors.New("google oauth: device authorization response is missing device_code/user_code")
	}
	if resp.VerificationURL == "" {
		resp.VerificationURL = googleDeviceVerificationURL
	}
	if resp.VerificationURLComplete == "" {
		resp.VerificationURLComplete = resp.VerificationURL
	}

	interval := time.Duration(resp.Interval) * time.Second
	if interval <= 0 {
		interval = googleDefaultPollInterval
	}
	expiresIn := time.Duration(resp.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = googleDefaultDeviceExpiry
	}

	return &DeviceAuthSession{
		DeviceCode:              resp.DeviceCode,
		UserCode:                resp.UserCode,
		VerificationURL:         resp.VerificationURL,
		VerificationURLComplete: resp.VerificationURLComplete,
		Interval:                interval,
		ExpiresIn:               expiresIn,
		ExpiresAt:               g.now().Add(expiresIn),
	}, nil
}

// PollDeviceAuth exchanges a device code for a token. It returns
// ErrGoogleAuthorizationPending / ErrGoogleSlowDown / ErrGoogleAccessDenied /
// ErrGoogleExpiredToken as appropriate so the caller can implement the RFC
// 8628 polling loop. On success the token is persisted.
func (g *GoogleTokenSource) PollDeviceAuth(ctx context.Context, deviceCode string) (*GoogleToken, error) {
	if strings.TrimSpace(deviceCode) == "" {
		return nil, errors.New("google oauth: empty device code")
	}
	form := url.Values{}
	form.Set("client_id", g.cfg.ClientID)
	form.Set("device_code", deviceCode)
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	if g.cfg.ClientSecret != "" {
		form.Set("client_secret", g.cfg.ClientSecret)
	}

	tok, err := g.exchange(ctx, form)
	if err != nil {
		return nil, err
	}
	if err := g.storeToken(tok); err != nil {
		return nil, err
	}
	return tok.clone(), nil
}

// AuthorizeDevice runs the complete device flow: start, show the code via
// onCode, then poll until success, denial, expiry or ctx cancellation. It is
// the one-time user authorization entry point.
func (g *GoogleTokenSource) AuthorizeDevice(ctx context.Context, onCode func(*DeviceAuthSession)) (*GoogleToken, error) {
	session, err := g.StartDeviceAuth(ctx)
	if err != nil {
		return nil, err
	}
	if onCode != nil {
		onCode(session)
	}

	interval := session.Interval
	if g.pollInterval > 0 {
		interval = g.pollInterval
	}

	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		if !session.ExpiresAt.IsZero() && g.now().After(session.ExpiresAt) {
			return nil, ErrGoogleExpiredToken
		}

		tok, err := g.PollDeviceAuth(ctx, session.DeviceCode)
		if err == nil {
			return tok, nil
		}
		switch {
		case errors.Is(err, ErrGoogleAuthorizationPending):
			continue
		case errors.Is(err, ErrGoogleSlowDown):
			interval += 5 * time.Second
			continue
		default:
			return nil, err
		}
	}
}

// AuthorizeDeviceInteractive is AuthorizeDevice with a human-readable
// instruction written to w. Only the user code is printed; tokens never are.
func (g *GoogleTokenSource) AuthorizeDeviceInteractive(ctx context.Context, w io.Writer) (*GoogleToken, error) {
	return g.AuthorizeDevice(ctx, func(s *DeviceAuthSession) {
		fmt.Fprintf(w, "Google authorization required.\n1. Open %s\n2. Enter code: %s\nWaiting for approval...\n",
			s.VerificationURL, s.UserCode)
	})
}

// ExchangeAuthCode completes an OAuth 2.0 authorization-code + PKCE flow and
// persists the resulting token. The code and verifier are never logged.
func (g *GoogleTokenSource) ExchangeAuthCode(ctx context.Context, code, verifier, redirectURI string) (*GoogleToken, error) {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(verifier) == "" || strings.TrimSpace(redirectURI) == "" {
		return nil, errors.New("google oauth: code, verifier and redirect uri are required")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("client_id", g.cfg.ClientID)
	if g.cfg.ClientSecret != "" {
		form.Set("client_secret", g.cfg.ClientSecret)
	}
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", verifier)
	tok, err := g.exchange(ctx, form)
	if err != nil {
		return nil, err
	}
	if err := g.storeToken(tok); err != nil {
		return nil, err
	}
	return tok.clone(), nil
}

// AccessToken implements TokenProvider. It returns the cached access token
// while it is valid and transparently refreshes it otherwise.
func (g *GoogleTokenSource) AccessToken(ctx context.Context) (string, error) {
	if err := g.ensureLoaded(); err != nil {
		return "", err
	}
	if tok, ok := g.validToken(); ok {
		return tok.AccessToken, nil
	}

	g.refreshMu.Lock()
	defer g.refreshMu.Unlock()

	// Another goroutine may have refreshed while we waited.
	if tok, ok := g.validToken(); ok {
		return tok.AccessToken, nil
	}

	g.mu.Lock()
	refreshToken := ""
	if g.token != nil {
		refreshToken = g.token.RefreshToken
	}
	g.mu.Unlock()
	if refreshToken == "" {
		return "", ErrGoogleNotAuthorized
	}

	tok, err := g.refreshToken(ctx, refreshToken)
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

func (g *GoogleTokenSource) validToken() (*GoogleToken, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token == nil || g.token.AccessToken == "" {
		return nil, false
	}
	if !g.now().Add(googleTokenRefreshSkew).Before(g.token.Expiry) {
		return nil, false
	}
	return g.token.clone(), true
}

// Refresh forces a refresh using the stored refresh token.
func (g *GoogleTokenSource) Refresh(ctx context.Context) (*GoogleToken, error) {
	if err := g.ensureLoaded(); err != nil {
		return nil, err
	}
	g.refreshMu.Lock()
	defer g.refreshMu.Unlock()

	g.mu.Lock()
	refreshToken := ""
	if g.token != nil {
		refreshToken = g.token.RefreshToken
	}
	g.mu.Unlock()
	if refreshToken == "" {
		return nil, ErrGoogleNotAuthorized
	}
	return g.refreshToken(ctx, refreshToken)
}

// refreshToken performs the refresh_token grant. Google may omit the refresh
// token on refresh; the existing one is retained in that case.
func (g *GoogleTokenSource) refreshToken(ctx context.Context, refreshToken string) (*GoogleToken, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", g.cfg.ClientID)
	form.Set("refresh_token", refreshToken)
	if g.cfg.ClientSecret != "" {
		form.Set("client_secret", g.cfg.ClientSecret)
	}

	tok, err := g.exchange(ctx, form)
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	if err := g.storeToken(tok); err != nil {
		return nil, err
	}
	return tok.clone(), nil
}

func (g *GoogleTokenSource) exchange(ctx context.Context, form url.Values) (*GoogleToken, error) {
	var resp googleTokenResponse
	if err := g.postForm(ctx, g.cfg.Endpoints.Token, form, &resp); err != nil {
		return nil, err
	}
	if resp.AccessToken == "" {
		return nil, errors.New("google oauth: token response is missing access_token")
	}

	tok := &GoogleToken{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		TokenType:    resp.TokenType,
		Expiry:       g.now().Add(time.Duration(resp.ExpiresIn) * time.Second),
		Scopes:       strings.Fields(resp.Scope),
		Email:        googleEmailFromIDToken(resp.IDToken),
		IDToken:      resp.IDToken,
	}
	if len(tok.Scopes) == 0 {
		tok.Scopes = append([]string(nil), g.cfg.Scopes...)
	}
	if tok.TokenType == "" {
		tok.TokenType = "Bearer"
	}
	return tok, nil
}

type googleTokenResponse struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	Scope            string `json:"scope"`
	TokenType        string `json:"token_type"`
	IDToken          string `json:"id_token"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// googleOAuthError carries Google's error code but deliberately keeps the
// description out of Error() so a response body can never leak a credential
// into a log line.
type googleOAuthError struct {
	Code        string
	Description string
}

func (e *googleOAuthError) Error() string {
	return "google oauth: " + e.Code
}

func (e *googleOAuthError) Unwrap() error {
	switch e.Code {
	case "authorization_pending":
		return ErrGoogleAuthorizationPending
	case "slow_down":
		return ErrGoogleSlowDown
	case "access_denied":
		return ErrGoogleAccessDenied
	case "expired_token":
		return ErrGoogleExpiredToken
	case "invalid_grant":
		return ErrGoogleRefreshRejected
	case "invalid_client":
		return ErrGoogleInvalidClient
	}
	return nil
}

func googleOAuthErrorFrom(code, description string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New("google oauth: authorization failed")
	}
	return &googleOAuthError{Code: code, Description: description}
}

func (g *GoogleTokenSource) postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	if err := validateGoogleEndpoint(endpoint); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("google oauth: cannot build request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "citation-one-google-sync/1")

	resp, err := g.httpClient().Do(req)
	if err != nil {
		// url.Error contains the endpoint but never the form body.
		return fmt.Errorf("google oauth: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, googleAuthMaxResponseBytes))
	if err != nil {
		return errors.New("google oauth: cannot read response")
	}

	var probe struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Error != "" {
		return googleOAuthErrorFrom(probe.Error, probe.ErrorDescription)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("google oauth: HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("google oauth: malformed response")
	}
	return nil
}

// googleEmailFromIDToken extracts the email claim for display only. The token
// signature is not verified here; it came directly from Google's TLS
// endpoint. Failures are non-fatal.
func googleEmailFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return strings.TrimSpace(claims.Email)
}

func (g *GoogleTokenSource) ensureLoaded() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.loaded {
		return g.loadErr
	}
	g.loaded = true
	tok, err := g.loadTokenLocked()
	if err != nil {
		g.loadErr = err
		return err
	}
	g.token = tok
	return nil
}

func (g *GoogleTokenSource) loadTokenLocked() (*GoogleToken, error) {
	path := g.TokenPath()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, errors.New("google oauth: cannot read token file")
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0077 != 0 {
		// Tighten a token file that was created with a looser umask.
		_ = os.Chmod(path, 0600)
	}

	var stored googleStoredToken
	if err := json.Unmarshal(b, &stored); err != nil {
		return nil, errors.New("google oauth: token file is not valid JSON")
	}
	if stored.Version > googleTokenFileVersion {
		return nil, errors.New("google oauth: token file version is not supported")
	}
	if stored.AccessToken == "" && stored.RefreshToken == "" {
		return nil, errors.New("google oauth: token file has no credentials")
	}

	tok := &GoogleToken{
		AccessToken:  stored.AccessToken,
		RefreshToken: stored.RefreshToken,
		TokenType:    stored.TokenType,
		Scopes:       append([]string(nil), stored.Scopes...),
		Email:        stored.Email,
	}
	if stored.Expiry != "" {
		if t, err := time.Parse(time.RFC3339Nano, stored.Expiry); err == nil {
			tok.Expiry = t
		}
	}
	return tok, nil
}

// LoadToken returns the stored token without refreshing it. A missing file
// yields ErrGoogleNotAuthorized.
func (g *GoogleTokenSource) LoadToken() (*GoogleToken, error) {
	if err := g.ensureLoaded(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token == nil {
		return nil, ErrGoogleNotAuthorized
	}
	return g.token.clone(), nil
}

// Status reports a redacted authorization summary.
func (g *GoogleTokenSource) Status() GoogleAuthStatus {
	_ = g.ensureLoaded()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token == nil || (g.token.AccessToken == "" && g.token.RefreshToken == "") {
		return GoogleAuthStatus{}
	}
	return GoogleAuthStatus{
		Authorized: g.token.RefreshToken != "",
		Email:      g.token.Email,
		Expiry:     g.token.Expiry,
		Scopes:     append([]string(nil), g.token.Scopes...),
	}
}

// SaveToken persists tok atomically with mode 0600. It is exported so a
// provisioning tool can import an existing authorization.
func (g *GoogleTokenSource) SaveToken(tok *GoogleToken) error {
	if tok == nil || (tok.AccessToken == "" && tok.RefreshToken == "") {
		return errors.New("google oauth: refusing to store an empty token")
	}
	stored := googleStoredToken{
		Version:      googleTokenFileVersion,
		ClientID:     g.cfg.ClientID,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenType:    tok.TokenType,
		Scopes:       append([]string(nil), tok.Scopes...),
		Email:        tok.Email,
		UpdatedAt:    g.now().UTC().Format(time.RFC3339),
	}
	if !tok.Expiry.IsZero() {
		stored.Expiry = tok.Expiry.UTC().Format(time.RFC3339Nano)
	}
	b, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return errors.New("google oauth: cannot encode token")
	}
	return writeAtomicFile(g.TokenPath(), b, 0600)
}

func (g *GoogleTokenSource) storeToken(tok *GoogleToken) error {
	if tok == nil || tok.AccessToken == "" {
		return errors.New("google oauth: refusing to store an empty token")
	}
	if err := g.SaveToken(tok); err != nil {
		return err
	}
	g.mu.Lock()
	g.token = tok.clone()
	g.loaded = true
	g.loadErr = nil
	g.mu.Unlock()
	return nil
}

// googleStoredToken is the on-disk representation. ID tokens are deliberately
// not persisted; only the display email is kept.
type googleStoredToken struct {
	Version      int      `json:"version"`
	ClientID     string   `json:"client_id,omitempty"`
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token,omitempty"`
	TokenType    string   `json:"token_type,omitempty"`
	Expiry       string   `json:"expiry,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	Email        string   `json:"email,omitempty"`
	UpdatedAt    string   `json:"updated_at"`
}
