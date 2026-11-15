//go:build youtube_dial

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

const (
	youtubeAppID       = "233637DE"
	youtubeDIALService = "urn:dial-multiscreen-org:service:dial:1"
	youtubeDIALID      = "uuid:0d3c4b0a-5972-4760-9e10-4f729b0b7758"
)

var youtubePairingCode = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,128}$`)
var youtubeVideoID = regexp.MustCompile(`^[a-zA-Z0-9_-]{11}$`)
var youtubeLaunchParams atomic.Pointer[string]

func registerYouTubeDIAL(mux *http.ServeMux) func() {
	mux.HandleFunc("GET /youtube/launch", youtubeReceiverRedirect)
	return func() {
		go func() {
			if err := serveYouTubeDIAL(); err != nil {
				log.Printf("YouTube DIAL: %v", err)
			}
		}()
	}
}

func youtubeReceiverRedirect(w http.ResponseWriter, r *http.Request) {
	target := "https://www.youtube.com/tv?castv=2.0"
	if params := youtubeLaunchParams.Swap(nil); params != nil {
		target += "&" + *params
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusFound)
}

func serveYouTubeDIAL() error {
	iface, err := net.InterfaceByName("wlan0")
	if err != nil {
		return err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return err
	}
	var ip net.IP
	for _, addr := range addrs {
		if n, ok := addr.(*net.IPNet); ok && n.IP.To4() != nil {
			ip = n.IP.To4()
			break
		}
	}
	if ip == nil {
		return errors.New("Wi-Fi has no IPv4 address")
	}
	host := net.JoinHostPort(ip.String(), "18769")
	base := "http://" + host
	in, err := net.ListenMulticastUDP("udp4", iface, &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900})
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	if err != nil {
		return err
	}
	defer out.Close()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, peer, err := in.ReadFromUDP(buf)
			if err != nil {
				return
			}
			query := strings.ToLower(string(buf[:n]))
			if !strings.HasPrefix(query, "m-search ") || !(strings.Contains(query, youtubeDIALService) || strings.Contains(query, "ssdp:all")) {
				continue
			}
			reply := fmt.Sprintf("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=30\r\nEXT:\r\nLOCATION: %s/device.xml\r\nSERVER: Linux/4.4 UPnP/1.1 DIAL/2.2\r\nST: %s\r\nUSN: %s::%s\r\n\r\n", base, youtubeDIALService, youtubeDIALID, youtubeDIALService)
			_, _ = out.WriteToUDP([]byte(reply), peer)
		}
	}()
	var launching atomic.Bool
	var session atomic.Pointer[youtubeDIALSession]
	mux := http.NewServeMux()
	mux.HandleFunc("GET /device.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		w.Header().Set("Application-URL", base+"/apps/")
		fmt.Fprintf(w, `<?xml version="1.0"?><root xmlns="urn:schemas-upnp-org:device-1-0"><specVersion><major>1</major><minor>0</minor></specVersion><device><deviceType>urn:dial-multiscreen-org:device:dial:1</deviceType><friendlyName>Citation YouTube</friendlyName><manufacturer>Harman Kardon</manufacturer><modelName>Citation One</modelName><UDN>%s</UDN><serviceList><service><serviceType>%s</serviceType><serviceId>urn:dial-multiscreen-org:serviceId:dial</serviceId><controlURL>/apps/</controlURL><eventSubURL></eventSubURL><SCPDURL></SCPDURL></service></serviceList></device></root>`, youtubeDIALID, youtubeDIALService)
	})
	appStatus := func(w http.ResponseWriter, _ *http.Request) {
		state, link := "stopped", ""
		transport := currentYouTubeTransport()
		if launching.Load() || transport != "" {
			state, link = "running", `<link rel="run" href="run"/>`
		}
		extra := ""
		if transport != "" {
			if current := session.Load(); current != nil && current.Transport == transport {
				if encoded, err := xml.Marshal(current); err == nil {
					extra = string(encoded)
				}
			}
		}
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, `<?xml version="1.0"?><service xmlns="urn:dial-multiscreen-org:schemas:dial" dialVer="2.1"><name>YouTube</name><options allowStop="false"/><state>%s</state>%s%s</service>`, state, link, extra)
	}
	mux.HandleFunc("GET /apps/YouTube", appStatus)
	mux.HandleFunc("GET /apps/YouTube/run", appStatus)
	mux.HandleFunc("POST /apps/YouTube", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid launch data", 400)
			return
		}
		code := r.Form.Get("pairingCode")
		if code == "" {
			code = r.Form.Get("pairing_code")
		}
		if !youtubePairingCode.MatchString(code) {
			http.Error(w, "pairing code required", 400)
			return
		}
		if !launching.CompareAndSwap(false, true) {
			http.Error(w, "launch in progress", 409)
			return
		}
		params := url.Values{"pairingCode": {code}}
		if theme := r.Form.Get("theme"); theme == "cl" || theme == "leanback" {
			params.Set("theme", theme)
		}
		if video := r.Form.Get("v"); youtubeVideoID.MatchString(video) {
			params.Set("v", video)
		}
		go func() {
			defer launching.Store(false)
			if err := launchYouTubeDIAL(code, params.Encode(), &session); err != nil {
				log.Printf("YouTube DIAL launch: %v", err)
			}
		}()
		w.Header().Set("Location", base+"/apps/YouTube/run")
		w.WriteHeader(http.StatusCreated)
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		switch origin {
		case "https://www.youtube.com", "https://m.youtube.com", "http://www.youtube.com", "http://m.youtube.com", "package:com.google.android.youtube", "package:com.google.ios.youtube":
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Expose-Headers", "Application-URL, Location")
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		case "":
		default:
			log.Printf("YouTube DIAL rejected origin %q", origin)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
	return (&http.Server{Addr: host, Handler: handler, ReadHeaderTimeout: 3 * time.Second}).ListenAndServe()
}

type youtubeDIALSession struct {
	XMLName   xml.Name `xml:"additionalData"`
	ScreenID  string   `xml:"screenId"`
	DeviceID  string   `xml:"deviceId,omitempty"`
	Token     string   `xml:"loungeToken"`
	Transport string   `xml:"-"`
}

func currentYouTubeTransport() string {
	c, status, err := youtubeCastStatus(time.Second)
	if err != nil {
		return ""
	}
	defer c.conn.Close()
	return youtubeCastTransport(status)
}

func youtubeCastStatus(timeout time.Duration) (castStatusClient, M, error) {
	const ns = "urn:x-cast:com.google.cast.receiver"
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", "127.0.0.1:8009", &tls.Config{InsecureSkipVerify: true})
	c := castStatusClient{conn}
	if err != nil {
		return c, nil, err
	}
	conn.SetDeadline(time.Now().Add(timeout))
	if err = c.connectApp("receiver-0"); err == nil {
		err = c.send("receiver-0", ns, M{"type": "GET_STATUS", "requestId": 1})
	}
	var status M
	if err == nil {
		status, err = c.receive(ns)
	}
	if err != nil {
		conn.Close()
	}
	return c, status, err
}

func youtubeCastTransport(status M) string {
	body, _ := status["status"].(map[string]any)
	apps, _ := body["applications"].([]any)
	for _, raw := range apps {
		app, _ := raw.(map[string]any)
		if app["appId"] == youtubeAppID {
			id, _ := app["transportId"].(string)
			return id
		}
	}
	return ""
}

func launchYouTubeDIAL(code, launchParams string, session *atomic.Pointer[youtubeDIALSession]) (resultErr error) {
	castMediaMu.Lock()
	defer castMediaMu.Unlock()
	status := readSpotifyNative()
	if status.playback().State == "PLAYING" {
		if err := spotifyNativeCommand(context.Background(), "pause", M{}); err != nil {
			return err
		}
		defer func() {
			if resultErr != nil {
				_ = spotifyNativeCommand(context.Background(), "resume", M{})
			}
		}()
	}
	c, state, err := youtubeCastStatus(2 * time.Second)
	if err != nil {
		return err
	}
	defer c.conn.Close()
	c.conn.SetDeadline(time.Now().Add(15 * time.Second))
	transport := youtubeCastTransport(state)
	if transport != "" && session.Load() == nil {
		const ns = "urn:x-cast:com.google.cast.receiver"
		if err := c.send("receiver-0", ns, M{"type": "STOP", "requestId": 2, "sessionId": transport}); err != nil {
			return err
		}
		if _, err := c.receive(ns); err != nil {
			return err
		}
		transport = ""
	}
	if transport == "" {
		const ns = "urn:x-cast:com.google.cast.receiver"
		youtubeLaunchParams.Store(&launchParams)
		defer youtubeLaunchParams.Store(nil)
		if err := c.send("receiver-0", ns, M{"type": "LAUNCH", "requestId": 3, "appId": youtubeAppID, "silentLaunch": true}); err != nil {
			return err
		}
		for transport == "" {
			result, err := c.receive(ns)
			if err != nil {
				return err
			}
			if result["type"] == "LAUNCH_ERROR" {
				return fmt.Errorf("Cast launch: %v", result["reason"])
			}
			transport = youtubeCastTransport(result)
		}
	}
	if err := c.connectApp(transport); err != nil {
		return err
	}
	const mdx = "urn:x-cast:com.google.youtube.mdx"
	if err := c.send(transport, mdx, M{"type": "getMdxSessionStatus"}); err != nil {
		return err
	}
	reply, err := c.receive(mdx)
	if err != nil {
		return err
	}
	data, _ := reply["data"].(map[string]any)
	screen, _ := data["screenId"].(string)
	device, _ := data["deviceId"].(string)
	if screen == "" {
		return errors.New("YouTube did not report a screen ID")
	}
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.PostForm("https://www.youtube.com/api/lounge/pairing/register_pairing_code", url.Values{
		"access_type": {"permanent"}, "pairing_code": {code}, "screen_id": {screen},
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("YouTube pairing: HTTP %d", resp.StatusCode)
	}
	log.Print("YouTube DIAL pairing registered")
	tokenResp, err := client.PostForm("https://www.youtube.com/api/lounge/pairing/get_lounge_token_batch", url.Values{"screen_ids": {screen}})
	if err != nil {
		return err
	}
	defer tokenResp.Body.Close()
	var tokens struct {
		Screens []struct {
			ScreenID, LoungeToken string
		}
	}
	if tokenResp.StatusCode != http.StatusOK || json.NewDecoder(tokenResp.Body).Decode(&tokens) != nil {
		return errors.New("YouTube did not return lounge credentials")
	}
	for _, item := range tokens.Screens {
		if item.ScreenID == screen && item.LoungeToken != "" {
			session.Store(&youtubeDIALSession{ScreenID: screen, DeviceID: device, Token: item.LoungeToken, Transport: transport})
			return nil
		}
	}
	return errors.New("YouTube did not return a lounge token for this screen")
}
