package main

import (
	"strings"
	"time"
)

// Echo guard timestamps come from CLOCK_MONOTONIC in microseconds, not Unix
// nanoseconds. Keep the conversion in one place for the follow-up timer.
func followUpDelay(nowUs, guardUntilUs int64) time.Duration {
	delay := time.Duration(guardUntilUs-nowUs)*time.Microsecond + 100*time.Millisecond
	if delay < 200*time.Millisecond {
		return 200 * time.Millisecond
	}
	return delay
}

// A speaking phase is not indefinitely meaningful activity: a broken Live
// stream may deliver a partial answer without turnComplete. Allow queued audio
// to drain before treating eight seconds without new audio as a stall.
func replyAudioStalled(phase string, lastAudio, now time.Time, playbackGuardActive bool) bool {
	return phase == "speaking" && !lastAudio.IsZero() && !playbackGuardActive && now.Sub(lastAudio) >= 8*time.Second
}

// Pure session-lifetime policy for the native Gemini session.
//
// These helpers contain no timers, locks or I/O so main.go can wire them and
// tests can exercise the rules directly. Thresholds and phrases are inferred
// from observed stock behaviour and the 2026-11-09 noise audit; they are not
// decompiled constants.

// meaningfulSessionActivity reports whether the session has real user work in
// flight and should therefore not be closed by the idle watchdog.
//
// A post-wake VAD flag alone is not activity: keyboard noise opens the gate and
// sets postWakeSpeech, which is exactly how noise kept sessions alive. It only
// counts while native input is still open (the user is speaking into the mic).
// An accepted transcript (meaningful), a pending tool call, or a model phase
// that is thinking/speaking is real activity.
func meaningfulSessionActivity(meaningful, postWakeSpeech, nativeInputActive, awaitingTool bool, phase string) bool {
	if meaningful || awaitingTool {
		return true
	}
	if postWakeSpeech && nativeInputActive {
		return true
	}
	return sessionPhaseActive(phase)
}

// sessionPhaseActive reports whether the model, not the user, still owns the
// turn. "listening" and "idle" are deliberately absent: an open microphone is
// not evidence that a user is present.
func sessionPhaseActive(phase string) bool {
	switch phase {
	case "thinking", "speaking", "announcing_music", "launching_music":
		return true
	default:
		return false
	}
}

// followUpQuestionPhrases are explicit invitations for another turn. This list
// is inferred from the model's Russian/English replies, not recovered from the
// stock binary.
var followUpQuestionPhrases = []string{
	"хотите ещё", "хотите еще", "хотите что", "что-нибудь ещё", "что-нибудь еще",
	"что-то ещё", "что-то еще", "чем ещё помочь", "чем еще помочь",
	"нужно что-то ещё", "нужно что-то еще", "есть ещё", "есть еще",
	"would you like", "do you want", "anything else", "shall i", "should i",
	"can i help", "may i help", "how can i help", "is there anything",
}

// followUpAllowedByReply conservatively decides whether a completed reply
// invites another user turn. This is an inference, not a decompiled fact: the
// stock NORMAL_WITH_FOLLOW_ON outcome is not exposed by the Gemini API, so the
// reply text is the only available signal. It returns true only when the
// trimmed reply ends with a question mark or contains an explicit follow-up
// question phrase. A plain statement ("Арбуз.") must not reopen the mic.
func followUpAllowedByReply(output string) bool {
	t := strings.TrimSpace(strings.ToLower(output))
	if t == "" {
		return false
	}
	// Closing quotes/brackets and sentence punctuation may follow the question
	// mark; the question mark itself is never trimmed.
	trimmed := strings.TrimRight(t, " \t\r\n.!…\"'»”’")
	if strings.HasSuffix(trimmed, "?") {
		return true
	}
	for _, phrase := range followUpQuestionPhrases {
		if strings.Contains(t, phrase) {
			return true
		}
	}
	return false
}

// shouldCloseAfterNoise reports whether a session that has produced no
// meaningful user activity for at least grace should be closed. A zero
// lastMeaningful means no accepted transcript has ever arrived, so a noise-only
// session is closable immediately. A non-positive grace also closes
// immediately; callers should pass the configured grace window.
func shouldCloseAfterNoise(lastMeaningful time.Time, now time.Time, grace time.Duration) bool {
	if lastMeaningful.IsZero() || grace <= 0 {
		return true
	}
	return !now.Before(lastMeaningful.Add(grace))
}
