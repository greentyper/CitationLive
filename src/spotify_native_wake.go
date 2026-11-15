package main

import (
	"context"
	"errors"
	"time"
)

// Activate the already linked Spotify provider, without sending a song, mood,
// title or playlist query to Google. Catalog selection remains Spotify Web.
func wakeNativeSpotifyProvider(ctx context.Context) error {
	nativeHandoff.Add(1)
	defer nativeHandoff.Add(-1)
	defer func() { _ = grpcCall("StopInteraction", []byte{8, 1}) }()
	silence, e := openTTSRedirect()
	if e != nil {
		return e
	}
	defer silence.close()
	// A vanished Connect device may leave a zombie Cast app stuck BUFFERING.
	// Relaunch only Spotify, retaining the native Cast service and audio mixer.
	if e = stopSpotifyCastApp(); e != nil {
		return e
	}
	if e = waitSpotifyRegistration(ctx, 500*time.Millisecond); e != nil {
		return e
	}
	if e = grpcCall("SendTextQuery", pbMessage(1, []byte("play music on Spotify"))); e != nil {
		return e
	}
	until := time.Now().Add(28 * time.Second)
	for time.Now().Before(until) {
		if e := ctx.Err(); e != nil {
			return e
		}
		devices, e := directSpotify.devicesContext(ctx)
		if e == nil {
			for _, d := range devices {
				if speakerDevice(d) && d.Active {
					// /devices can appear before the receiver has usable media.
					// Do not interrupt provider startup at that intermediate point.
					if playback, err := readPlaybackStatus(); err == nil && playback.State == "PLAYING" {
						directSpotify.bindDeviceID(d.ID)
						return nil
					}
				}
			}
		}
		if e := waitSpotifyRegistration(ctx, 500*time.Millisecond); e != nil {
			return e
		}
	}
	return errors.New("native Spotify provider did not register the speaker")
}
