package main

func (c *controller) setPhase(phase string) {
	c.mu.Lock()
	changed := c.state != phase
	c.state = phase
	if cfg.TestTrace && c.stats != nil {
		trace, _ := c.stats["test_phases"].([]M)
		if len(trace) == 0 || trace[len(trace)-1]["phase"] != phase {
			c.stats["test_phases"] = append(trace, M{"phase": phase, "monotonic_us": monotonicMicros()})
		}
	}
	c.mu.Unlock()
	if !changed && phase != "idle" {
		return
	}
	nativeOwnsLights := applyNativePhase(phase)
	switch phase {
	case "listening":
		_ = ducking.DuckListening()
	case "speaking":
		_ = ducking.Duck()
	case "idle":
		_ = ducking.Restore()
	}
	if nativeOwnsLights {
		return
	}
	name := map[string]string{"connecting": "003_thinking", "opening_audio": "003_thinking", "listening": "002_listening", "thinking": "003_thinking", "announcing_music": "003_thinking", "launching_music": "003_thinking", "speaking": "004_responding", "idle": "000_idle"}[phase]
	if name != "" {
		if phase == "idle" {
			led("animation_close")
		}
		led(name)
	}
}
