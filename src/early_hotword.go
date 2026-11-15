package main

import (
	"bufio"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const earlyHotwordLibrary = "/data/opt/citation-gemini/bin/libassistant.hotword-observe-v3.so"

func earlyHotwordInstalled() bool {
	patched, err := os.Stat(earlyHotwordLibrary)
	if err != nil {
		return false
	}
	live, err := os.Stat("/chrome/lib/libassistant.so")
	return err == nil && os.SameFile(patched, live)
}

func earlyHotwordEvent(line string, since, now time.Time) bool {
	if !strings.Contains(line, ":INFO:hotword_detector.cc(503)] Hotword detected with mic status:") {
		return false
	}
	stamp, _, _ := strings.Cut(strings.TrimSpace(line), " ")
	seconds, fraction, ok := strings.Cut(stamp, ".")
	if !ok || len(fraction) != 3 {
		return false
	}
	sec, secErr := strconv.ParseInt(seconds, 10, 64)
	ms, msErr := strconv.ParseInt(fraction, 10, 64)
	at := time.Unix(sec, ms*int64(time.Millisecond))
	return secErr == nil && msErr == nil && !at.Before(since) &&
		now.Sub(at) <= time.Second && !at.After(now)
}

func watchEarlyHotword() {
	var lastWake time.Time
	for {
		if !earlyHotwordInstalled() {
			time.Sleep(time.Second)
			continue
		}
		cmd := exec.Command("/bin/logcat", "-T", "1", "-v", "epoch", "cast_shell:I", "*:S")
		out, err := cmd.StdoutPipe()
		since := time.Now().Truncate(time.Millisecond)
		if err != nil {
			log.Println("early hotword log pipe:", err)
		} else if err = cmd.Start(); err != nil {
			log.Println("early hotword log start:", err)
		} else {
			scanner := bufio.NewScanner(out)
			scanner.Buffer(make([]byte, 8192), 256*1024)
			for scanner.Scan() {
				now := time.Now()
				if !earlyHotwordEvent(scanner.Text(), since, now) ||
					now.Sub(lastWake) < 1500*time.Millisecond ||
					now.UnixNano() < cancelledWakeUntil.Load() ||
					nativeHandoff.Load() != 0 ||
					!earlyHotwordInstalled() {
					continue
				}
				if wakeSuppressed() {
					// The native detector can hear our own TTS/cue. Cancel the
					// Google interaction it just opened, but do not start Gemini.
					go func() { _ = grpcCall("StopInteraction", []byte{8, 1}) }()
					continue
				}
				state, err := wampCallTimeout(250*time.Millisecond, "com.harman.stateGet", M{})
				if err != nil || len(state) < 5 {
					continue
				}
				groups, _ := state[4].(map[string]any)
				mic, _ := groups["microphone"].(map[string]any)
				if mic["state"] != "mic-unmuted" {
					continue
				}
				lastWake = now
				go ctl.restartForEarlyHotword()
			}
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		time.Sleep(time.Second)
	}
}

func (c *controller) restartForEarlyHotword() {
	if c.busy() {
		c.stop()
		// stop only cancels. The old session still owns the controller
		// until capture and playback cleanup have both finished.
		deadline := time.Now().Add(2 * time.Second)
		for c.busy() {
			if time.Now().After(deadline) {
				log.Println("native hotword restart timed out waiting for the previous session")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if time.Now().UnixNano() < cancelledWakeUntil.Load() {
		return
	}
	log.Println("native hotword accepted before Google interaction")
	c.start()
}
