package main

import (
	"context"
	"testing"
)

func TestBroadcastResponseConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  M
		want bool
	}{
		{"reported no speakers", M{"ok": true, "assistant_response": "Sorry, it looks like there aren't any speakers connected."}, false},
		{"curly apostrophe", M{"ok": true, "assistant_response": "There aren’t any speakers connected."}, false},
		{"empty", M{"ok": true}, false},
		{"generic ack", M{"ok": true, "assistant_response": "OK"}, false},
		{"question", M{"ok": true, "assistant_response": "What would you like to broadcast?"}, false},
		{"negated", M{"ok": true, "assistant_response": "Your message was not broadcast."}, false},
		{"conditional", M{"ok": true, "assistant_response": "Your message was broadcast if speakers were available."}, false},
		{"mixed failure", M{"ok": true, "assistant_response": "OK, broadcasting now. Sorry, something went wrong."}, false},
		{"quoted acknowledgement", M{"ok": true, "assistant_response": "You said: Your message was broadcast."}, false},
		{"transport failure", M{"ok": false, "assistant_response": "Your message was broadcast."}, false},
		{"error field", M{"ok": true, "error": "timeout", "assistant_response": "Your message was broadcast."}, false},
		{"transcription failure", M{"ok": true, "transcribe_error": "partial", "assistant_response": "Your message was broadcast."}, false},
		{"broadcasting", M{"ok": true, "assistant_response": "Okay, broadcasting now."}, true},
		{"sent", M{"ok": true, "assistant_response": "Your message has been broadcast."}, true},
		{"Russian sent", M{"ok": true, "assistant_response": "Сообщение отправлено на все устройства."}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := broadcastResponseConfirmed(tc.res); got != tc.want {
				t.Fatalf("confirmed=%v, want %v: %#v", got, tc.want, tc.res)
			}
		})
	}
}

func TestBroadcastExecution(t *testing.T) {
	for _, tc := range []struct {
		name, message                        string
		verified, canceled, wantCall, wantOK bool
		response                             M
	}{
		{name: "empty", message: "  ", verified: true},
		{name: "unverified", message: "dinner"},
		{name: "canceled", message: "dinner", verified: true, canceled: true},
		{name: "no devices", message: "dinner", verified: true, wantCall: true, response: M{"ok": true, "assistant_response": "Sorry, there aren't any speakers connected."}},
		{name: "ambiguous", message: "dinner", verified: true, wantCall: true, response: M{"ok": true, "assistant_response": "Sure"}},
		{name: "confirmed", message: " dinner ", verified: true, wantCall: true, wantOK: true, response: M{"ok": true, "assistant_response": "OK, broadcasting now."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			calls := 0
			res := executeBroadcast(ctx, tc.message, func() bool { return tc.verified }, func(_ context.Context, query string) M {
				calls++
				if query != "broadcast dinner" {
					t.Fatalf("unexpected query %q", query)
				}
				return tc.response
			})
			if (calls == 1) != tc.wantCall || calls > 1 {
				t.Fatalf("calls=%d", calls)
			}
			if (res["ok"] == true) != tc.wantOK {
				t.Fatalf("result=%#v", res)
			}
			if res["local_broadcast"] == true {
				t.Fatal("unexpected local fallback")
			}
			if !tc.wantOK && (res["google_broadcast"] == true || res["error"] == nil) {
				t.Fatalf("failure claimed success or lacks explanation: %#v", res)
			}
		})
	}
}

func TestBroadcastToolScope(t *testing.T) {
	if res, handled := handleBroadcastTool(context.Background(), "other", nil); handled || res != nil {
		t.Fatal("claimed unrelated tool")
	}
}
