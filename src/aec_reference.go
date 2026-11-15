package main

import (
	"sync"
	"time"
)

var aecReference struct {
	sync.Mutex
	ref *mixerReference
}

// Keep speaker-output history while idle, so pre-roll and cloud-handshake
// capture have an aligned echo reference too. Contains only a bounded PCM ring.
func sharedAECReference() (*mixerReference, error) {
	aecReference.Lock()
	defer aecReference.Unlock()
	if r := aecReference.ref; r != nil {
		r.mu.Lock()
		bad := r.lastError != ""
		r.mu.Unlock()
		if !bad {
			return r, nil
		}
		r.conn.Close()
		aecReference.ref = nil
	}
	r, err := openMixerReference()
	if err == nil {
		aecReference.ref = r
	}
	return r, err
}
func keepAECReference() {
	for {
		_, _ = sharedAECReference()
		time.Sleep(time.Second)
	}
}
