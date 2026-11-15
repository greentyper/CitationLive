package main

import (
	"bufio"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

const (
	djWakeBinary = "/tmp/citation-kws/citation-dj-wake"
	djWakeHMM    = "/tmp/citation-kws/en-us"
	djWakeDict   = "/tmp/citation-kws/dj.dict"
	djWakeKWS    = "/tmp/citation-kws/dj.kws"
)

var djWakeTriggeredAtUs atomic.Int64

func isRecentDJWake() bool {
	at := djWakeTriggeredAtUs.Load()
	if at == 0 {
		return false
	}
	diff := monotonicMicros() - at
	return diff >= 0 && diff <= 4000000
}

func djWakeInstalled() bool {
	if _, err := os.Stat(djWakeBinary); err != nil {
		return false
	}
	if _, err := os.Stat(djWakeHMM + "/mdef"); err != nil {
		return false
	}
	if _, err := os.Stat(djWakeDict); err != nil {
		return false
	}
	if _, err := os.Stat(djWakeKWS); err != nil {
		return false
	}
	return true
}

func watchDJWake() {
	if !cfg.DJWake {
		return
	}
	var lastWake time.Time
	for {
		if !djWakeInstalled() || !globalCaptureWorker.isRunning() {
			time.Sleep(time.Second)
			continue
		}

		sub, err := globalCaptureWorker.subscribe(0)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}

		cmd := exec.Command(djWakeBinary, djWakeHMM, djWakeDict, djWakeKWS)
		inPipe, inErr := cmd.StdinPipe()
		outPipe, outErr := cmd.StdoutPipe()
		if inErr != nil || outErr != nil {
			sub.close()
			time.Sleep(time.Second)
			continue
		}
		if err := cmd.Start(); err != nil {
			log.Println("dj wake start:", err)
			sub.close()
			time.Sleep(2 * time.Second)
			continue
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			scanner := bufio.NewScanner(outPipe)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if !strings.HasPrefix(line, "WAKE:") {
					continue
				}
				now := time.Now()
				if now.Sub(lastWake) < 1500*time.Millisecond ||
					now.UnixNano() < cancelledWakeUntil.Load() ||
					nativeHandoff.Load() != 0 {
					continue
				}
				if wakeSuppressed() {
					continue
				}
				if ctl.busy() {
					log.Println("DJ wake word received while busy; interrupting prior session")
					ctl.stop()
					deadline := time.Now().Add(500 * time.Millisecond)
					for ctl.busy() && time.Now().Before(deadline) {
						time.Sleep(20 * time.Millisecond)
					}
				}
				state, err := wampCallTimeout(100*time.Millisecond, "com.harman.stateGet", M{})
				if err == nil && len(state) >= 5 {
					if groups, ok := state[4].(map[string]any); ok {
						if mic, ok := groups["microphone"].(map[string]any); ok {
							if mic["state"] == "mic-muted" {
								continue
							}
						}
					}
				}
				lastWake = now
				djWakeTriggeredAtUs.Store(monotonicMicros())
				log.Println("custom DJ hotword detected:", line)
				go ctl.start()
			}
		}()

		var dcFilter microphoneDCFilter
		preRoll := make([][]byte, 0, 50)
		speechTailFrames := 0
		activeFrames := 0
		var noiseFloor float64 = 8.0
		baseGain := microphoneGain()
		if usingNativeCaptureService() && baseGain < 6 {
			baseGain = 6
		}
		flushFrame := make([]byte, 320)
		for i := 0; i < 320; i += 2 {
			flushFrame[i] = 0xfe
			flushFrame[i+1] = 0x7f
		}

		for {
			select {
			case <-done:
				goto restart
			default:
			}
			frame, _, rErr := sub.read()
			if rErr != nil {
				break
			}
			if ctl.busy() {
				preRoll = preRoll[:0]
				speechTailFrames = 0
				activeFrames = 0
				continue
			}

			buf := make([]byte, len(frame))
			copy(buf, frame)
			dcFilter.apply(buf)
			rms := nativeFrameACRMS(buf)
			gainPCM16(buf, baseGain)

			if speechTailFrames == 0 {
				if rms < noiseFloor*1.5 {
					noiseFloor = noiseFloor*0.98 + rms*0.02
				} else {
					noiseFloor = noiseFloor*0.998 + rms*0.002
				}
			}
			vadThreshold := noiseFloor * 1.5
			if vadThreshold < 12.0 {
				vadThreshold = 12.0
			}
			if vadThreshold > 30.0 {
				vadThreshold = 30.0
			}

			if rms > vadThreshold {
				if speechTailFrames == 0 && len(preRoll) > 0 {
					for _, old := range preRoll {
						if _, wErr := inPipe.Write(old); wErr != nil {
							goto restart
						}
					}
					activeFrames += len(preRoll)
					preRoll = preRoll[:0]
				}
				speechTailFrames = 50 // keep feeding for 500ms after speech energy drops
			}

			if speechTailFrames > 0 {
				speechTailFrames--
				activeFrames++
				if _, wErr := inPipe.Write(buf); wErr != nil {
					break
				}
				if speechTailFrames == 0 || activeFrames >= 400 {
					// Send utterance-end sentinel so PocketSphinx updates CMN and flushes any pending detection
					if _, wErr := inPipe.Write(flushFrame); wErr != nil {
						goto restart
					}
					speechTailFrames = 0
					activeFrames = 0
				}
			} else {
				preRoll = append(preRoll, buf)
				if len(preRoll) > 50 {
					preRoll = preRoll[1:]
				}
			}
		}

	restart:
		sub.close()
		_ = inPipe.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		time.Sleep(time.Second)
	}
}
