package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"sync"
	"time"
)

// duckingManager manages native audio ducking via Cast StreamMixer SetVolumeLimit.
type duckingManager struct {
	mu                sync.Mutex
	isDucked          bool
	ducked            bool // alias for compatibility
	explicitPaused    bool
	explicitlyPaused  bool // alias for compatibility
	currentMultiplier float32
	sendLimitFn       func(contentType int, multiplier float32) error
	pauseFn           func() error
	resumeFn          func() error
	appliedLimit      float32
	restorePending    bool
	control           net.Conn
	lastWrite         time.Time
	startupSilences   int
}

// SetVolumeLimit is an absolute ceiling, NOT a relative gain. Native saved
// volumes contain the actual dBFS gain; use it without changing user volume.
func duckCeiling(data []byte, factor float32) (float32, error) {
	var v struct {
		DBFS struct {
			Media *float64 `json:"media"`
		} `json:"dbfs"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return 0, err
	}
	if v.DBFS.Media == nil || math.IsNaN(*v.DBFS.Media) || math.IsInf(*v.DBFS.Media, 0) || *v.DBFS.Media > 0 {
		return 0, fmt.Errorf("invalid native media dBFS")
	}
	return float32(math.Pow(10, *v.DBFS.Media/20)) * factor, nil
}

func (d *duckingManager) writeLimit(factor float32) error {
	if d.startupSilences > 0 {
		factor = 0
	}
	if d.sendLimitFn != nil {
		return d.sendLimitFn(0, factor)
	}
	limit := factor
	if factor > 0 && factor < 1 {
		data, err := os.ReadFile("/data/chrome/saved_volumes")
		if err != nil {
			return err
		}
		limit, err = duckCeiling(data, factor)
		if err != nil {
			return err
		}
	}
	// Native Google focus release can overwrite this global ceiling after our
	// write. Local cached equality is not evidence of the mixer's current value.
	if limit == d.appliedLimit && (factor == 1 || time.Since(d.lastWrite) < 250*time.Millisecond) {
		return nil
	}
	if d.control == nil {
		conn, err := mixerDial()
		if err != nil {
			return err
		}
		d.control = conn
	}
	d.control.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	if err := mixerProto(d.control, pbSetVolumeLimit(0, limit)); err != nil {
		d.control.Close()
		d.control = nil
		return err
	}
	d.appliedLimit = limit
	d.lastWrite = time.Now()
	if factor == 1 {
		d.control.Close()
		d.control = nil
	}
	return nil
}

// Follow native volume changes while holding our ceiling. Never write the
// user's 0..48 volume; Gemini's OTHER focus is not limited by MEDIA ducking.
func (d *duckingManager) refreshUntilShutdown() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		d.mu.Lock()
		if d.startupSilences > 0 {
			if err := d.writeLimit(0); err != nil {
				log.Println("Spotify startup silence refresh:", clean(err))
			}
		} else if d.restorePending {
			if d.writeLimit(1) == nil {
				d.restorePending = false
				d.isDucked, d.ducked = false, false
				d.currentMultiplier = 1
			}
		} else if d.isDucked {
			if err := d.writeLimit(d.currentMultiplier); err != nil {
				log.Println("duck refresh:", clean(err))
			}
		}
		d.mu.Unlock()
	}
}

// Hold MEDIA at zero without redirecting/rebuilding the Spotify audio stream.
// Ordinary duck/restore calls still update the desired post-startup level.
func (d *duckingManager) holdSpotifyStartupSilence() (func() error, error) {
	d.mu.Lock()
	d.startupSilences++
	if err := d.writeLimit(0); err != nil {
		d.startupSilences--
		d.mu.Unlock()
		return nil, err
	}
	d.mu.Unlock()
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.startupSilences--
			factor := float32(1)
			if d.isDucked {
				factor = d.currentMultiplier
			}
			releaseErr = d.writeLimit(factor)
			if releaseErr != nil {
				d.restorePending = true
			}
		})
		return releaseErr
	}, nil
}

// newDuckingManager constructs a duckingManager.
func newDuckingManager(sendLimit func(int, float32) error, pause func() error, resume ...func() error) *duckingManager {
	var r func() error
	if len(resume) > 0 {
		r = resume[0]
	}
	return &duckingManager{
		currentMultiplier: 1.0,
		sendLimitFn:       sendLimit,
		pauseFn:           pause,
		resumeFn:          r,
	}
}

// newDuckingState is an alias for newDuckingManager.
func newDuckingState(sendLimit func(int, float32) error, pause func() error, resume func() error) *duckingManager {
	return newDuckingManager(sendLimit, pause, resume)
}

// duckingState is a type alias for duckingManager.
type duckingState = duckingManager

// Global singleton instance used by the daemon.
var ducking = newDuckingManager(nil, nil)

// Duck attenuates MEDIA content type to 0.2 (150ms hardware fade down).
// It is idempotent if already ducked.
func (d *duckingManager) Duck() error          { return d.duckAt(0.2) }
func (d *duckingManager) DuckListening() error { return d.duckAt(0) }
func (d *duckingManager) duckAt(factor float32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.explicitPaused = false
	d.explicitlyPaused = false
	d.restorePending = false
	if d.isDucked && d.currentMultiplier == factor {
		return nil
	}
	err := d.writeLimit(factor)
	if err != nil {
		log.Println("native ducking error:", clean(err))
		return err
	}
	d.isDucked = true
	d.ducked = true
	d.currentMultiplier = factor
	return nil
}

// Restore restores MEDIA content type to 1.0 (700ms hardware fade up) unless explicitly paused.
func (d *duckingManager) Restore() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.isDucked {
		return nil
	}
	err := d.writeLimit(1)
	if err != nil {
		// The logical state must still be restored so a later cleanup/turn is
		// not stuck ducked. refreshUntilShutdown retries the native write.
		d.isDucked = false
		d.ducked = false
		d.currentMultiplier = 1.0
		d.restorePending = true
		log.Println("native unducking error:", clean(err))
		return err
	}
	d.isDucked = false
	d.ducked = false
	d.currentMultiplier = 1.0
	d.restorePending = false
	return nil
}

// OnExplicitPause marks the session as explicitly paused, restores volume multiplier to 1.0,
// and executes pause without auto-resume.
func (d *duckingManager) OnExplicitPause() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.explicitPaused = true
	d.explicitlyPaused = true
	limitErr := d.writeLimit(1)
	d.restorePending = limitErr != nil
	if limitErr == nil {
		d.isDucked = false
		d.ducked = false
		d.currentMultiplier = 1
	}

	if limitErr != nil {
		log.Println("native explicit pause limit restore error:", clean(limitErr))
	}

	var pauseErr error
	if d.pauseFn != nil {
		pauseErr = d.pauseFn()
	} else {
		_, pauseErr = wampCall("com.harman.gva.pause")
	}
	if pauseErr != nil {
		log.Println("native explicit pause call error:", clean(pauseErr))
		return pauseErr
	}
	return limitErr
}

// ExplicitPause is an alias for OnExplicitPause.
func (d *duckingManager) ExplicitPause() error {
	return d.OnExplicitPause()
}

// OnSessionEnd cleans up any active ducking state on session termination.
func (d *duckingManager) OnSessionEnd() error {
	return d.Restore()
}

// Reset restores volume multiplier to 1.0 and clears all ducking state on daemon boot.
func (d *duckingManager) Reset() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.writeLimit(1); err != nil {
		d.restorePending = true
		return err
	}
	d.isDucked = false
	d.ducked = false
	d.explicitPaused = false
	d.explicitlyPaused = false
	d.currentMultiplier = 1
	d.restorePending = false
	return nil
}
