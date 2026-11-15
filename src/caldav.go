package main

// CalDAV fallback client for Google Calendar.
//
// Google exposes Calendar over CalDAV at
//   https://apidata.googleusercontent.com/caldav/v2/<calendar-id>/events/
// using HTTP Basic auth with an account email and a Google "app password".
// This client implements the subset needed for sync: PROPFIND (discovery),
// REPORT (calendar-query), GET, PUT (create/update) and DELETE.
//
// Security invariants:
//   - HTTPS is mandatory; credentials are never sent over plaintext.
//   - Credentials are never logged (String/GoString redact the password).
//   - Requests never leave the configured host, so Basic auth cannot be
//     redirected to a third party.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	caldavCredentialsFileName = "caldav.json"
	caldavContentType         = "text/calendar; charset=utf-8"
	caldavXMLContentType      = "application/xml; charset=utf-8"
	caldavMaxResponseBytes    = 8 << 20
	caldavDefaultHTTPTimeout  = 30 * time.Second
)

// Environment variables consulted by LoadCalDAVConfigFrom.
const (
	envCalDAVBaseURL   = "CITATION_CALDAV_BASE_URL"
	envCalDAVUsername  = "CITATION_CALDAV_USERNAME"
	envCalDAVPassword  = "CITATION_CALDAV_APP_PASSWORD"
	envCalDAVPassword2 = "CITATION_CALDAV_PASSWORD"
)

// CalDAV sentinel errors.
var (
	ErrCalDAVNotFound           = errors.New("caldav: resource not found")
	ErrCalDAVPreconditionFailed = errors.New("caldav: etag precondition failed")
	ErrCalDAVUnauthorized       = errors.New("caldav: unauthorized")
	ErrCalDAVInsecure           = errors.New("caldav: https is required")
	ErrCalDAVCrossHost          = errors.New("caldav: refusing to send credentials to a different host")
)

// CalDAVConfig configures a CalDAVClient. Empty credential fields are filled
// from <state>/caldav.json.
type CalDAVConfig struct {
	BaseURL    string
	Username   string
	Password   string // Google app password
	HTTPClient *http.Client
	StateDir   string
	Now        func() time.Time
}

// CalDAVCredentials is the on-disk credential record. Password is redacted
// from String()/GoString().
type CalDAVCredentials struct {
	BaseURL  string `json:"base_url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (c CalDAVCredentials) String() string {
	return fmt.Sprintf("CalDAVCredentials{base_url:%s username:%s password:[redacted]}", c.BaseURL, c.Username)
}

func (c CalDAVCredentials) GoString() string { return c.String() }

// CalDAVCollection is one PROPFIND response entry.
type CalDAVCollection struct {
	Href        string
	DisplayName string
	ETag        string
	IsCalendar  bool
}

// CalDAVEvent is a VEVENT plus its CalDAV resource metadata.
type CalDAVEvent struct {
	Href        string
	ETag        string
	UID         string
	Summary     string
	Description string
	Location    string
	Status      string
	Start       time.Time
	End         time.Time
	AllDay      bool
	Stamp       time.Time
	Raw         string
}

// CalDAVClient is safe for concurrent use.
type CalDAVClient struct {
	baseURL  *url.URL
	username string
	password string
	http     *http.Client
	now      func() time.Time
	stateDir string
}

// LoadCalDAVConfig reads credentials from the environment, falling back to
// <state>/caldav.json.
func LoadCalDAVConfig() (CalDAVConfig, error) {
	return LoadCalDAVConfigFrom(stateDirectory)
}

// LoadCalDAVConfigFrom is LoadCalDAVConfig with an explicit state directory.
func LoadCalDAVConfigFrom(dir string) (CalDAVConfig, error) {
	cfg := CalDAVConfig{StateDir: dir}
	if dir == "" {
		cfg.StateDir = stateDirectory
	}
	if creds, err := LoadCalDAVCredentialsFrom(cfg.StateDir); err == nil {
		cfg.BaseURL = creds.BaseURL
		cfg.Username = creds.Username
		cfg.Password = creds.Password
	}
	if v := firstNonEmptyEnv(envCalDAVBaseURL); v != "" {
		cfg.BaseURL = v
	}
	if v := firstNonEmptyEnv(envCalDAVUsername); v != "" {
		cfg.Username = v
	}
	if v := firstNonEmptyEnv(envCalDAVPassword, envCalDAVPassword2); v != "" {
		cfg.Password = v
	}
	if cfg.BaseURL == "" || cfg.Username == "" || cfg.Password == "" {
		return cfg, errors.New("caldav: credentials are not configured")
	}
	return cfg, nil
}

func caldavCredentialsPath(dir string) string {
	if dir == "" {
		dir = stateDirectory
	}
	return filepath.Join(dir, caldavCredentialsFileName)
}

// LoadCalDAVCredentials reads <state>/caldav.json.
func LoadCalDAVCredentials() (CalDAVCredentials, error) {
	return LoadCalDAVCredentialsFrom(stateDirectory)
}

// LoadCalDAVCredentialsFrom reads credentials from dir, tightening file
// permissions to 0600 if needed.
func LoadCalDAVCredentialsFrom(dir string) (CalDAVCredentials, error) {
	path := caldavCredentialsPath(dir)
	b, err := os.ReadFile(path)
	if err != nil {
		return CalDAVCredentials{}, errors.New("caldav: cannot read credentials file")
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0077 != 0 {
		_ = os.Chmod(path, 0600)
	}
	var creds CalDAVCredentials
	if err := json.Unmarshal(b, &creds); err != nil {
		return CalDAVCredentials{}, errors.New("caldav: credentials file is not valid JSON")
	}
	return creds, nil
}

// SaveCalDAVCredentials writes <state>/caldav.json atomically with mode 0600.
func SaveCalDAVCredentials(creds CalDAVCredentials) error {
	return SaveCalDAVCredentialsTo(stateDirectory, creds)
}

// SaveCalDAVCredentialsTo is SaveCalDAVCredentials with an explicit dir.
func SaveCalDAVCredentialsTo(dir string, creds CalDAVCredentials) error {
	creds.BaseURL = strings.TrimSpace(creds.BaseURL)
	creds.Username = strings.TrimSpace(creds.Username)
	if creds.BaseURL == "" || creds.Username == "" || creds.Password == "" {
		return errors.New("caldav: base url, username and app password are required")
	}
	u, err := url.Parse(creds.BaseURL)
	if err != nil || u.Host == "" {
		return errors.New("caldav: invalid base url")
	}
	if u.Scheme != "https" {
		return ErrCalDAVInsecure
	}
	if u.User != nil {
		return errors.New("caldav: base url must not embed credentials")
	}
	b, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return errors.New("caldav: cannot encode credentials")
	}
	return writeAtomicFile(caldavCredentialsPath(dir), b, 0600)
}

// NewCalDAVClient validates configuration and returns a client.
func NewCalDAVClient(cfg CalDAVConfig) (*CalDAVClient, error) {
	dir := cfg.StateDir
	if dir == "" {
		dir = stateDirectory
	}
	if cfg.BaseURL == "" || cfg.Username == "" || cfg.Password == "" {
		if creds, err := LoadCalDAVCredentialsFrom(dir); err == nil {
			if cfg.BaseURL == "" {
				cfg.BaseURL = creds.BaseURL
			}
			if cfg.Username == "" {
				cfg.Username = creds.Username
			}
			if cfg.Password == "" {
				cfg.Password = creds.Password
			}
		}
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("caldav: base url is not configured")
	}
	if cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("caldav: username and app password are required")
	}

	base, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || base.Host == "" {
		return nil, errors.New("caldav: invalid base url")
	}
	if base.Scheme != "https" {
		return nil, ErrCalDAVInsecure
	}
	if base.User != nil {
		return nil, errors.New("caldav: base url must not embed credentials")
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}

	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	return &CalDAVClient{
		baseURL:  base,
		username: cfg.Username,
		password: cfg.Password,
		http:     httpsOnlyClient(cfg.HTTPClient),
		now:      now,
		stateDir: dir,
	}, nil
}

// BaseURL returns the configured calendar collection URL.
func (c *CalDAVClient) BaseURL() string { return c.baseURL.String() }

func httpsOnlyClient(base *http.Client) *http.Client {
	if base == nil {
		base = &http.Client{Timeout: caldavDefaultHTTPTimeout}
	}
	cloned := *base
	previous := cloned.CheckRedirect
	cloned.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return ErrCalDAVInsecure
		}
		if len(via) >= 10 {
			return errors.New("caldav: too many redirects")
		}
		if previous != nil {
			return previous(req, via)
		}
		return nil
	}
	return &cloned
}

func (c *CalDAVClient) resolve(target string) (*url.URL, error) {
	if target == "" {
		return c.baseURL, nil
	}
	ref, err := url.Parse(target)
	if err != nil {
		return nil, errors.New("caldav: invalid url")
	}
	u := c.baseURL.ResolveReference(ref)
	if u.Scheme != "https" {
		return nil, ErrCalDAVInsecure
	}
	if !strings.EqualFold(u.Host, c.baseURL.Host) {
		return nil, ErrCalDAVCrossHost
	}
	return u, nil
}

func (c *CalDAVClient) newRequest(ctx context.Context, method, target string, body []byte, headers map[string]string) (*http.Request, error) {
	u, err := c.resolve(target)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, errors.New("caldav: cannot build request")
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("User-Agent", "citation-one-caldav/1")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

func (c *CalDAVClient) send(req *http.Request) (*http.Response, []byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("caldav: request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, caldavMaxResponseBytes))
	if err != nil {
		return resp, nil, errors.New("caldav: cannot read response")
	}
	return resp, body, nil
}

func caldavStatusError(op string, resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("caldav: %s: %w", op, ErrCalDAVUnauthorized)
	case http.StatusForbidden:
		return fmt.Errorf("caldav: %s: forbidden", op)
	case http.StatusNotFound, http.StatusGone:
		return fmt.Errorf("caldav: %s: %w", op, ErrCalDAVNotFound)
	case http.StatusPreconditionFailed:
		return fmt.Errorf("caldav: %s: %w", op, ErrCalDAVPreconditionFailed)
	default:
		return fmt.Errorf("caldav: %s: HTTP %d", op, resp.StatusCode)
	}
}

const caldavPropfindBody = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:cs="http://calendarserver.org/ns/">
  <d:prop>
    <d:current-user-principal/>
    <d:resourcetype/>
    <d:displayname/>
    <d:getetag/>
    <c:supported-calendar-component-set/>
    <cs:getctag/>
  </d:prop>
</d:propfind>`

// Discover issues PROPFIND Depth: 1 against the configured collection.
func (c *CalDAVClient) Discover(ctx context.Context) ([]CalDAVCollection, error) {
	return c.DiscoverAt(ctx, "")
}

// DiscoverAt issues PROPFIND against calendarURL (empty means the base URL).
func (c *CalDAVClient) DiscoverAt(ctx context.Context, calendarURL string) ([]CalDAVCollection, error) {
	req, err := c.newRequest(ctx, "PROPFIND", calendarURL, []byte(caldavPropfindBody), map[string]string{
		"Depth":        "1",
		"Content-Type": caldavXMLContentType,
	})
	if err != nil {
		return nil, err
	}
	resp, body, err := c.send(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusMultiStatus && resp.StatusCode != http.StatusOK {
		return nil, caldavStatusError("propfind", resp)
	}
	return parseCalDAVCollections(body, c.baseURL)
}

// ListCalendars is an alias for Discover.
func (c *CalDAVClient) ListCalendars(ctx context.Context) ([]CalDAVCollection, error) {
	return c.Discover(ctx)
}

func caldavQueryBody(start, end time.Time) []byte {
	if end.IsZero() {
		end = start.Add(24 * time.Hour)
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
  <d:prop>
    <d:getetag/>
    <c:calendar-data/>
  </d:prop>
  <c:filter>
    <c:comp-filter name="VCALENDAR">
      <c:comp-filter name="VEVENT">
        <c:time-range start="%s" end="%s"/>
      </c:comp-filter>
    </c:comp-filter>
  </c:filter>
</c:calendar-query>`, start.UTC().Format(icsUTCFormat), end.UTC().Format(icsUTCFormat)))
}

// ListEvents issues a calendar-query REPORT for [start, end).
func (c *CalDAVClient) ListEvents(ctx context.Context, calendarURL string, start, end time.Time) ([]CalDAVEvent, error) {
	req, err := c.newRequest(ctx, "REPORT", calendarURL, caldavQueryBody(start, end), map[string]string{
		"Depth":        "1",
		"Content-Type": caldavXMLContentType,
	})
	if err != nil {
		return nil, err
	}
	resp, body, err := c.send(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusMultiStatus && resp.StatusCode != http.StatusOK {
		return nil, caldavStatusError("report", resp)
	}
	return parseCalDAVEvents(body, c.baseURL)
}

// QueryEvents is an alias for ListEvents.
func (c *CalDAVClient) QueryEvents(ctx context.Context, calendarURL string, start, end time.Time) ([]CalDAVEvent, error) {
	return c.ListEvents(ctx, calendarURL, start, end)
}

// GetEvent fetches a single VEVENT resource.
func (c *CalDAVClient) GetEvent(ctx context.Context, href string) (*CalDAVEvent, error) {
	if strings.TrimSpace(href) == "" {
		return nil, errors.New("caldav: event href is required")
	}
	req, err := c.newRequest(ctx, http.MethodGet, href, nil, map[string]string{
		"Accept": caldavContentType,
	})
	if err != nil {
		return nil, err
	}
	resp, body, err := c.send(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, caldavStatusError("get", resp)
	}
	events := parseICSEvents(string(body))
	if len(events) == 0 {
		return nil, errors.New("caldav: response contains no VEVENT")
	}
	ev := events[0]
	ev.Href = href
	ev.ETag = strings.TrimSpace(resp.Header.Get("ETag"))
	ev.Raw = string(body)
	return &ev, nil
}

// CreateEvent PUTs a new VEVENT with If-None-Match: *.
func (c *CalDAVClient) CreateEvent(ctx context.Context, calendarURL string, ev *CalDAVEvent) (*CalDAVEvent, error) {
	if ev == nil {
		return nil, errors.New("caldav: nil event")
	}
	if ev.UID == "" {
		ev.UID = newCalDAVUID()
	}
	if ev.Start.IsZero() {
		return nil, errors.New("caldav: event start is required")
	}
	if ev.End.IsZero() {
		if ev.AllDay {
			ev.End = ev.Start.AddDate(0, 0, 1)
		} else {
			ev.End = ev.Start.Add(time.Hour)
		}
	}

	base := calendarURL
	if base == "" {
		base = c.baseURL.String()
	}
	href := strings.TrimSuffix(base, "/") + "/" + url.PathEscape(ev.UID) + ".ics"

	payload := ev.ICS()
	req, err := c.newRequest(ctx, http.MethodPut, href, payload, map[string]string{
		"Content-Type":  caldavContentType,
		"If-None-Match": "*",
	})
	if err != nil {
		return nil, err
	}
	resp, _, err := c.send(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return nil, caldavStatusError("create", resp)
	}

	created := *ev
	created.Href = href
	created.Raw = string(payload)
	if etag := strings.TrimSpace(resp.Header.Get("ETag")); etag != "" {
		created.ETag = etag
	}
	return &created, nil
}

// UpdateEvent PUTs an existing VEVENT with If-Match when an ETag is known.
func (c *CalDAVClient) UpdateEvent(ctx context.Context, ev *CalDAVEvent) (*CalDAVEvent, error) {
	if ev == nil {
		return nil, errors.New("caldav: nil event")
	}
	if strings.TrimSpace(ev.Href) == "" {
		return nil, errors.New("caldav: event href is required for update")
	}
	if ev.Start.IsZero() {
		return nil, errors.New("caldav: event start is required")
	}
	if ev.End.IsZero() {
		if ev.AllDay {
			ev.End = ev.Start.AddDate(0, 0, 1)
		} else {
			ev.End = ev.Start.Add(time.Hour)
		}
	}

	headers := map[string]string{"Content-Type": caldavContentType}
	if ev.ETag != "" {
		headers["If-Match"] = ev.ETag
	}
	payload := ev.ICS()
	req, err := c.newRequest(ctx, http.MethodPut, ev.Href, payload, headers)
	if err != nil {
		return nil, err
	}
	resp, _, err := c.send(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return nil, caldavStatusError("update", resp)
	}

	updated := *ev
	updated.Raw = string(payload)
	if etag := strings.TrimSpace(resp.Header.Get("ETag")); etag != "" {
		updated.ETag = etag
	}
	return &updated, nil
}

// DeleteEvent DELETEs a VEVENT. An already-absent resource (404/410) is
// treated as success so sync is idempotent.
func (c *CalDAVClient) DeleteEvent(ctx context.Context, href, etag string) error {
	if strings.TrimSpace(href) == "" {
		return errors.New("caldav: event href is required")
	}
	headers := map[string]string{}
	if etag != "" {
		headers["If-Match"] = etag
	}
	req, err := c.newRequest(ctx, http.MethodDelete, href, nil, headers)
	if err != nil {
		return err
	}
	resp, _, err := c.send(req)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound, http.StatusGone:
		return nil
	}
	return caldavStatusError("delete", resp)
}

// --- WebDAV / iCalendar parsing ---

type caldavMultiStatus struct {
	XMLName   xml.Name         `xml:"DAV: multistatus"`
	Responses []caldavResponse `xml:"DAV: response"`
}

type caldavResponse struct {
	Href      string           `xml:"DAV: href"`
	Propstats []caldavPropstat `xml:"DAV: propstat"`
}

type caldavPropstat struct {
	Status string     `xml:"DAV: status"`
	Prop   caldavProp `xml:"DAV: prop"`
}

type caldavProp struct {
	DisplayName  string        `xml:"DAV: displayname"`
	GetETag      string        `xml:"DAV: getetag"`
	ResourceType caldavResType `xml:"DAV: resourcetype"`
	CalendarData string        `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
}

type caldavResType struct {
	Collection *struct{} `xml:"DAV: collection"`
	Calendar   *struct{} `xml:"urn:ietf:params:xml:ns:caldav calendar"`
}

func parseCalDAVCollections(raw []byte, base *url.URL) ([]CalDAVCollection, error) {
	var ms caldavMultiStatus
	if err := xml.Unmarshal(raw, &ms); err != nil {
		return nil, errors.New("caldav: malformed multistatus XML")
	}
	out := make([]CalDAVCollection, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		col := CalDAVCollection{
			Href: caldavResolveHref(base, r.Href),
		}
		for _, ps := range r.Propstats {
			if col.DisplayName == "" {
				col.DisplayName = strings.TrimSpace(ps.Prop.DisplayName)
			}
			if col.ETag == "" {
				col.ETag = strings.TrimSpace(ps.Prop.GetETag)
			}
			if ps.Prop.ResourceType.Calendar != nil {
				col.IsCalendar = true
			}
		}
		out = append(out, col)
	}
	return out, nil
}

func parseCalDAVEvents(raw []byte, base *url.URL) ([]CalDAVEvent, error) {
	var ms caldavMultiStatus
	if err := xml.Unmarshal(raw, &ms); err != nil {
		return nil, errors.New("caldav: malformed multistatus XML")
	}
	var out []CalDAVEvent
	for _, r := range ms.Responses {
		href := caldavResolveHref(base, r.Href)
		etag := ""
		data := ""
		for _, ps := range r.Propstats {
			if etag == "" {
				etag = strings.TrimSpace(ps.Prop.GetETag)
			}
			if data == "" && strings.TrimSpace(ps.Prop.CalendarData) != "" {
				data = ps.Prop.CalendarData
			}
		}
		if strings.TrimSpace(data) == "" {
			continue
		}
		for _, ev := range parseICSEvents(data) {
			ev.Href = href
			ev.ETag = etag
			ev.Raw = data
			out = append(out, ev)
		}
	}
	return out, nil
}

func caldavResolveHref(base *url.URL, href string) string {
	href = strings.TrimSpace(href)
	if href == "" || base == nil {
		return href
	}
	ref, err := url.Parse(href)
	if err != nil {
		return href
	}
	return base.ResolveReference(ref).String()
}

const icsUTCFormat = "20060102T150405Z"

var (
	icsUTCLayouts   = []string{"20060102T150405Z", "20060102T1504Z"}
	icsLocalLayouts = []string{"20060102T150405", "20060102T1504"}
	icsDateLayout   = "20060102"
)

func unfoldICS(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	var logical []string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(logical) > 0 {
			logical[len(logical)-1] += line[1:]
			continue
		}
		logical = append(logical, line)
	}
	return logical
}

// parseICSEvents extracts VEVENTs from an iCalendar payload.
func parseICSEvents(raw string) []CalDAVEvent {
	var out []CalDAVEvent
	var cur *CalDAVEvent
	for _, line := range unfoldICS(raw) {
		switch strings.ToUpper(line) {
		case "BEGIN:VEVENT":
			cur = &CalDAVEvent{}
			continue
		case "END:VEVENT":
			if cur != nil {
				out = append(out, *cur)
			}
			cur = nil
			continue
		}
		if cur == nil {
			continue
		}
		name, params, value := splitICSProperty(line)
		switch name {
		case "UID":
			cur.UID = unescapeICSText(value)
		case "SUMMARY":
			cur.Summary = unescapeICSText(value)
		case "DESCRIPTION":
			cur.Description = unescapeICSText(value)
		case "LOCATION":
			cur.Location = unescapeICSText(value)
		case "STATUS":
			cur.Status = strings.ToUpper(strings.TrimSpace(value))
		case "DTSTART":
			if t, allDay, ok := parseICSDate(value, params); ok {
				cur.Start = t
				cur.AllDay = allDay
			}
		case "DTEND":
			if t, _, ok := parseICSDate(value, params); ok {
				cur.End = t
			}
		case "DTSTAMP":
			if t, _, ok := parseICSDate(value, params); ok {
				cur.Stamp = t
			}
		}
	}
	return out
}

func splitICSProperty(line string) (string, map[string]string, string) {
	colon := -1
	inQuote := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuote = !inQuote
		case ':':
			if !inQuote {
				colon = i
			}
		}
		if colon >= 0 {
			break
		}
	}
	if colon < 0 {
		return "", nil, line
	}
	head := line[:colon]
	value := line[colon+1:]

	parts := splitICSHeader(head)
	name := strings.ToUpper(strings.TrimSpace(parts[0]))
	params := make(map[string]string)
	for _, p := range parts[1:] {
		eq := strings.IndexByte(p, '=')
		if eq <= 0 {
			continue
		}
		key := strings.ToUpper(strings.TrimSpace(p[:eq]))
		val := strings.Trim(strings.TrimSpace(p[eq+1:]), `"`)
		params[key] = val
	}
	return name, params, value
}

func splitICSHeader(head string) []string {
	var parts []string
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(head); i++ {
		switch head[i] {
		case '"':
			inQuote = !inQuote
			b.WriteByte(head[i])
		case ';':
			if inQuote {
				b.WriteByte(head[i])
			} else {
				parts = append(parts, b.String())
				b.Reset()
			}
		default:
			b.WriteByte(head[i])
		}
	}
	parts = append(parts, b.String())
	return parts
}

func parseICSDate(value string, params map[string]string) (time.Time, bool, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false, false
	}

	loc := time.UTC
	if tzid := params["TZID"]; tzid != "" {
		if l, err := time.LoadLocation(tzid); err == nil {
			loc = l
		}
	}

	if len(value) == 8 && !strings.Contains(value, "T") {
		if t, err := time.ParseInLocation(icsDateLayout, value, loc); err == nil {
			return t, true, true
		}
		return time.Time{}, false, false
	}

	for _, layout := range icsUTCLayouts {
		if t, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return t, false, true
		}
	}
	for _, layout := range icsLocalLayouts {
		if t, err := time.ParseInLocation(layout, value, loc); err == nil {
			return t, false, true
		}
	}
	return time.Time{}, false, false
}

func unescapeICSText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n', 'N':
				b.WriteByte('\n')
				i++
			case ',':
				b.WriteByte(',')
				i++
			case ';':
				b.WriteByte(';')
				i++
			case '\\':
				b.WriteByte('\\')
				i++
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func escapeICSText(s string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		";", "\\;",
		",", "\\,",
		"\r\n", "\\n",
		"\n", "\\n",
		"\r", "\\n",
	)
	return replacer.Replace(s)
}

// foldICSLine folds a content line at 75 octets without splitting a UTF-8
// rune, per RFC 5545 section 3.1.
func foldICSLine(line string) []string {
	const maxOctets = 75
	var out []string
	var b strings.Builder
	used := 0
	for _, r := range line {
		n := utf8.RuneLen(r)
		if used+n > maxOctets {
			out = append(out, b.String())
			b.Reset()
			b.WriteByte(' ')
			used = 1
		}
		b.WriteRune(r)
		used += n
	}
	out = append(out, b.String())
	return out
}

// ICS serializes the event as an iCalendar object.
func (e *CalDAVEvent) ICS() []byte {
	var b strings.Builder
	write := func(line string) {
		for _, part := range foldICSLine(line) {
			b.WriteString(part)
			b.WriteString("\r\n")
		}
	}

	stamp := e.Stamp
	if stamp.IsZero() {
		stamp = time.Now()
	}

	write("BEGIN:VCALENDAR")
	write("VERSION:2.0")
	write("PRODID:-//Citation ONE//Google Sync//EN")
	write("CALSCALE:GREGORIAN")
	write("BEGIN:VEVENT")
	write("UID:" + escapeICSText(e.UID))
	write("DTSTAMP:" + stamp.UTC().Format(icsUTCFormat))
	if e.AllDay {
		write("DTSTART;VALUE=DATE:" + e.Start.Format("20060102"))
		end := e.End
		if end.IsZero() || !end.After(e.Start) {
			end = e.Start.AddDate(0, 0, 1)
		}
		write("DTEND;VALUE=DATE:" + end.Format("20060102"))
	} else {
		write("DTSTART:" + e.Start.UTC().Format(icsUTCFormat))
		end := e.End
		if end.IsZero() {
			end = e.Start.Add(time.Hour)
		}
		write("DTEND:" + end.UTC().Format(icsUTCFormat))
	}
	if e.Summary != "" {
		write("SUMMARY:" + escapeICSText(e.Summary))
	}
	if e.Description != "" {
		write("DESCRIPTION:" + escapeICSText(e.Description))
	}
	if e.Location != "" {
		write("LOCATION:" + escapeICSText(e.Location))
	}
	if e.Status != "" {
		write("STATUS:" + strings.ToUpper(e.Status))
	}
	write("END:VEVENT")
	write("END:VCALENDAR")
	return []byte(b.String())
}

func newCalDAVUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d@citation-one", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf) + "@citation-one"
}
