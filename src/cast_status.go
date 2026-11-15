package main

import (
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"time"
)

type playbackStatus struct {
	App        string  `json:"app"`
	State      string  `json:"state"`
	Title      string  `json:"title"`
	Artist     string  `json:"artist"`
	Playlist   string  `json:"playlist,omitempty"`
	ContextURI string  `json:"context_uri,omitempty"`
	ContentID  string  `json:"-"`
	Session    float64 `json:"-"`
	Position   float64 `json:"-"`
}

type castStatusClient struct{ conn net.Conn }

func (c *castStatusClient) send(destination, namespace string, payload M) error {
	b, e := json.Marshal(payload)
	if e != nil {
		return e
	}
	var p []byte
	p = append(p, pbInt(1, 0)...)
	for _, f := range []struct {
		n int
		s string
	}{{2, "sender-0"}, {3, destination}, {4, namespace}} {
		p = append(p, pbMessage(f.n, []byte(f.s))...)
	}
	p = append(p, pbInt(5, 0)...)
	p = append(p, pbMessage(6, b)...)
	h := make([]byte, 4)
	binary.BigEndian.PutUint32(h, uint32(len(p)))
	_, e = c.conn.Write(append(h, p...))
	return e
}

func (c *castStatusClient) connectApp(destination string) error {
	payload := M{
		"type":      "CONNECT",
		"origin":    M{},
		"userAgent": "PyChromecast",
		"senderInfo": M{
			"sdkType":        2,
			"version":        "15.605.1.3",
			"browserVersion": "44.0.2403.30",
			"platform":       4,
			"systemVersion":  "Macintosh; Intel Mac OS X10_10_3",
			"connectionType": 1,
		},
	}
	return c.send(destination, "urn:x-cast:com.google.cast.tp.connection", payload)
}

func (c *castStatusClient) receive(namespace string) (M, error) {
	for {
		var h [4]byte
		if _, e := io.ReadFull(c.conn, h[:]); e != nil {
			return nil, e
		}
		n := binary.BigEndian.Uint32(h[:])
		if n > 1<<20 {
			return nil, errors.New("oversized Cast message")
		}
		b := make([]byte, n)
		if _, e := io.ReadFull(c.conn, b); e != nil {
			return nil, e
		}
		fields, e := pbDecode(b)
		if e != nil {
			return nil, e
		}
		ns := string(fields[4].data)
		var payload M
		if json.Unmarshal(fields[6].data, &payload) != nil {
			continue
		}
		log.Printf("Cast message namespace=%s type=%v", ns, payload["type"])
		if ns == "urn:x-cast:com.google.cast.tp.heartbeat" && payload["type"] == "PING" {
			if e = c.send(string(fields[2].data), ns, M{"type": "PONG"}); e != nil {
				return nil, e
			}
		}
		if ns == namespace {
			return payload, nil
		}
	}
}

func (c *castStatusClient) receiveAny() (string, M, error) {
	var h [4]byte
	if _, e := io.ReadFull(c.conn, h[:]); e != nil {
		return "", nil, e
	}
	n := binary.BigEndian.Uint32(h[:])
	if n > 1<<20 {
		return "", nil, errors.New("oversized Cast message")
	}
	b := make([]byte, n)
	if _, e := io.ReadFull(c.conn, b); e != nil {
		return "", nil, e
	}
	fields, e := pbDecode(b)
	if e != nil {
		return "", nil, e
	}
	ns := string(fields[4].data)
	var payload M
	_ = json.Unmarshal(fields[6].data, &payload)
	log.Printf("Cast message namespace=%s type=%v", ns, payload["type"])
	if ns == "urn:x-cast:com.google.cast.tp.heartbeat" && payload["type"] == "PING" {
		_ = c.send(string(fields[2].data), ns, M{"type": "PONG"})
	}
	return ns, payload, nil
}

func readPlaybackStatus() (playbackStatus, error) {
	var out playbackStatus
	// The local Cast receiver uses its own self-signed device certificate.
	conn, e := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", "127.0.0.1:8009", &tls.Config{InsecureSkipVerify: true})
	if e != nil {
		return out, e
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(4 * time.Second))
	c := castStatusClient{conn}
	const control = "urn:x-cast:com.google.cast.tp.connection"
	const receiver = "urn:x-cast:com.google.cast.receiver"
	const media = "urn:x-cast:com.google.cast.media"
	if e = c.send("receiver-0", control, M{"type": "CONNECT"}); e != nil {
		return out, e
	}
	if e = c.send("receiver-0", receiver, M{"type": "GET_STATUS", "requestId": 1}); e != nil {
		return out, e
	}
	r, e := c.receive(receiver)
	if e != nil {
		return out, e
	}
	status, _ := r["status"].(map[string]any)
	apps, _ := status["applications"].([]any)
	var transport string
	for _, entry := range apps {
		app, _ := entry.(map[string]any)
		if name, _ := app["displayName"].(string); name == "Spotify" {
			out.App = name
			transport, _ = app["transportId"].(string)
			break
		}
	}
	if transport == "" {
		return out, errors.New("Spotify receiver is not active")
	}
	if e = c.send(transport, control, M{"type": "CONNECT"}); e != nil {
		return out, e
	}
	if e = c.send(transport, media, M{"type": "GET_STATUS", "requestId": 2}); e != nil {
		return out, e
	}
	r, e = c.receive(media)
	if e != nil {
		return out, e
	}
	statuses, _ := r["status"].([]any)
	if len(statuses) == 0 {
		return out, errors.New("Spotify has no media status")
	}
	s, _ := statuses[0].(map[string]any)
	out.State, _ = s["playerState"].(string)
	out.Session, _ = s["mediaSessionId"].(float64)
	out.Position, _ = s["currentTime"].(float64)
	m, _ := s["media"].(map[string]any)
	out.ContentID, _ = m["contentId"].(string)
	metadata, _ := m["metadata"].(map[string]any)
	out.Title, _ = metadata["title"].(string)
	out.Artist, _ = metadata["artist"].(string)
	return out, nil
}

// Stop only the Spotify receiver application for a controlled cold-start test;
// the Cast service and its permanently open hardware output are untouched.
func stopSpotifyCastApp() error {
	conn, e := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", "127.0.0.1:8009", &tls.Config{InsecureSkipVerify: true})
	if e != nil {
		return e
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(4 * time.Second))
	c := castStatusClient{conn}
	const control = "urn:x-cast:com.google.cast.tp.connection"
	const receiver = "urn:x-cast:com.google.cast.receiver"
	if e = c.send("receiver-0", control, M{"type": "CONNECT"}); e != nil {
		return e
	}
	if e = c.send("receiver-0", receiver, M{"type": "GET_STATUS", "requestId": 1}); e != nil {
		return e
	}
	reply, e := c.receive(receiver)
	if e != nil {
		return e
	}
	status, _ := reply["status"].(map[string]any)
	apps, _ := status["applications"].([]any)
	for _, entry := range apps {
		app, _ := entry.(map[string]any)
		if app["appId"] == "CC32E753" {
			sid, _ := app["sessionId"].(string)
			if sid == "" {
				return errors.New("Spotify Cast session ID missing")
			}
			return c.send("receiver-0", receiver, M{"type": "STOP", "sessionId": sid, "requestId": 2})
		}
	}
	return nil
}
