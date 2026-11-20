package main

import "time"

// One mutating native command per observed user speech revision. Server tool
// continuations cannot repeatedly change volume without fresh input.
type commandGate struct {
	inputRevision, consumedRevision uint64
	lastCommand                     time.Time
	utteranceOpen                   bool
	actions                         map[string]commandAction
}
type commandAction struct {
	revision uint64
	at       time.Time
}

func (g *commandGate) allowAction(now time.Time, name string) bool {
	family := map[string]string{"set_volume": "volume", "adjust_volume": "volume", "media_control": "media", "play_music": "music", "spotify_dj_prompt": "music", "speaker_bluetooth": "bluetooth", "spotify_playback": "spotify_playback", "set_home_city": "preferences", "end_session": "session"}[name]
	if family == "" {
		return true
	}
	if g.actions == nil {
		g.actions = map[string]commandAction{}
	}
	last := g.actions[family]
	if g.inputRevision == 0 || last.revision == g.inputRevision || (!last.at.IsZero() && now.Sub(last.at) < 2*time.Second) {
		return false
	}
	g.actions[family] = commandAction{g.inputRevision, now}
	return true
}

func (g *commandGate) input() {
	if !g.utteranceOpen {
		g.inputRevision++
		g.utteranceOpen = true
	}
}
func (g *commandGate) end() { g.utteranceOpen = false }
func (g *commandGate) allow(now time.Time) bool {
	if g.inputRevision == g.consumedRevision || (!g.lastCommand.IsZero() && now.Sub(g.lastCommand) < 2*time.Second) {
		return false
	}
	g.consumedRevision = g.inputRevision
	g.lastCommand = now
	return true
}
