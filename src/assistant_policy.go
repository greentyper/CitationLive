package main

// Reimplemented policy pieces recovered from the shipped libassistant.so.
//
// The stock library exposes no C++ symbols, but its string table still contains
// the enum labels, feature flags and log messages of the assistant state
// machine. The code below is a behavioural rewrite of those observable rules,
// not a copy of the closed-source implementation. Every inferred threshold is
// marked as such; the enum names are observed strings.

import (
	"math"
	"sync"
	"time"
)

// conversationState is the state set printed by the stock conversation state
// event. The concatenated stock string is:
// END_OF_SPEECH ERROR_STATE EXECUTING IDLE LISTENING START_OF_SPEECH THINKING
// TTS_PLAYING UNKNOWN_STATE
type conversationState string

const (
	conversationIdle          conversationState = "IDLE"
	conversationListening     conversationState = "LISTENING"
	conversationThinking      conversationState = "THINKING"
	conversationExecuting     conversationState = "EXECUTING"
	conversationTTSPlaying    conversationState = "TTS_PLAYING"
	conversationStartOfSpeech conversationState = "START_OF_SPEECH"
	conversationEndOfSpeech   conversationState = "END_OF_SPEECH"
	conversationError         conversationState = "ERROR_STATE"
	conversationUnknown       conversationState = "UNKNOWN_STATE"
)

// conversationOutcome is the stock final result of an interaction. The
// important distinction for us is NORMAL_WITH_FOLLOW_ON: the server explicitly
// asks for another turn. A plain NORMAL must not keep the microphone open.
type conversationOutcome string

const (
	outcomeBargeIn            conversationOutcome = "BARGE_IN"
	outcomeCancelled          conversationOutcome = "CANCELLED"
	outcomeCommunicationError conversationOutcome = "COMMUNICATION_ERROR"
	outcomeDeviceNotSelected  conversationOutcome = "DEVICE_NOT_SELECTED"
	outcomeNormal             conversationOutcome = "NORMAL"
	outcomeNormalWithFollowOn conversationOutcome = "NORMAL_WITH_FOLLOW_ON"
	outcomeNoResponse         conversationOutcome = "NO_RESPONSE"
	outcomeTimeout            conversationOutcome = "TIMEOUT"
	outcomeUnknown            conversationOutcome = "UNKNOWN"
)

// followOnAllowed is an inference, not a decompiled fact. Ghidra 12.1.4 found
// no direct xref to NORMAL_WITH_FOLLOW_ON; its numeric ordinal and the code
// that acts on it remain unproven. This model keeps the conversation open only
// for that outcome.
func followOnAllowed(outcome conversationOutcome) bool {
	return outcome == outcomeNormalWithFollowOn
}

// interactionSource is the stock trigger/source enum:
// ALWAYS_ON CONVERSATION HOTWORD MAGIC_MIC NON_INTERRUPTING STORYBOOK TAP
// UNKNOWN, plus the MIC_REOPEN / MICROPHONE_TAP source used by the speech
// activity.
type interactionSource string

const (
	sourceAlwaysOn        interactionSource = "ALWAYS_ON"
	sourceConversation    interactionSource = "CONVERSATION"
	sourceHotword         interactionSource = "HOTWORD"
	sourceMagicMic        interactionSource = "MAGIC_MIC"
	sourceMicrophoneTap   interactionSource = "MICROPHONE_TAP"
	sourceMicReopen       interactionSource = "MIC_REOPEN"
	sourceNonInterrupting interactionSource = "NON_INTERRUPTING"
	sourceStorybook       interactionSource = "STORYBOOK"
	sourceTap             interactionSource = "TAP"
	sourceUnknown         interactionSource = "UNKNOWN"
)

// isBargeIn matches the stock MAGIC_MIC path. MAGIC_MIC is allowed to reopen
// the microphone immediately, even while ASR is active. MIC_REOPEN is a
// separate scheduled-reopen source and remains subject to the ASR/rate-limit
// checks.
func (s interactionSource) isBargeIn() bool {
	return s == sourceMagicMic
}

func (s interactionSource) isReopen() bool {
	return s == sourceMicReopen
}

// requiresHotword matches the sources that the stock assistant gates on a
// hotword before starting a normal query.
func (s interactionSource) requiresHotword() bool {
	switch s {
	case sourceAlwaysOn, sourceConversation, sourceHotword:
		return true
	default:
		return false
	}
}

// interruptsTTS matches the stock interruption paths. NON_INTERRUPTING and
// STORYBOOK deliberately do not appear here.
func (s interactionSource) interruptsTTS() bool {
	switch s {
	case sourceHotword, sourceMagicMic, sourceMicReopen, sourceMicrophoneTap, sourceTap:
		return true
	default:
		return false
	}
}

// micReopenDecision is the result of the stock microphone-reopen policy.
type micReopenDecision string

const (
	micReopenAllowed                  micReopenDecision = "ALLOWED"
	micReopenIgnoredASRActive         micReopenDecision = "IGNORED_ASR_ACTIVE"
	micReopenIgnoredHoldToTalk        micReopenDecision = "IGNORED_HOLD_TO_TALK"
	micReopenIgnoredRateLimit         micReopenDecision = "IGNORED_RATE_LIMITED"
	micReopenIgnoredListeningDisabled micReopenDecision = "IGNORED_LISTENING_DISABLED"
	micReopenImmediateBargeIn         micReopenDecision = "IMMEDIATE_BARGE_IN"
)

// micReopenContext is the state the stock ScheduleReopenMicrophone function
// checks before scheduling a reopen. ExistingReopen is not a blocking state:
// the stock aborts the old request and continues through the remaining gates.
type micReopenContext struct {
	ListeningEnabled bool
	ExistingReopen   bool
	HoldToTalk       bool
	ASRActive        bool
	ASRStopping      bool
}

// micReopenPolicy mirrors the stock max_mic_reopen_requests /
// mic_reopen_window_size / enable_limit_mic_reopen controls. Ghidra 12.1.4
// showed the exact gate order: existing reopen (abort and continue) ->
// listening disabled -> hold-to-talk -> ASR active/stopping -> rate limit.
// The stock rate limiter is an 8-entry timestamp ring with a configurable
// window; the default numbers were not in the string table.
type micReopenPolicy struct {
	maxRequests     int
	window          time.Duration
	requests        []time.Time
	existingAborted bool
}

func newMicReopenPolicy(maxRequests int, window time.Duration) *micReopenPolicy {
	if window <= 0 {
		window = 3 * time.Second
	}
	return &micReopenPolicy{maxRequests: maxRequests, window: window}
}

// decide is the convenience wrapper used by main.go and tests.
func (p *micReopenPolicy) decide(now time.Time, source interactionSource, asrActive, asrStopping, holdToTalk bool) micReopenDecision {
	return p.decideWithContext(now, source, micReopenContext{
		ListeningEnabled: true,
		HoldToTalk:       holdToTalk,
		ASRActive:        asrActive,
		ASRStopping:      asrStopping,
	})
}

// decideWithContext applies the stock ordering. Barge-in is a separate
// immediate path (FUN_004129d8) and is kept as an early return here.
func (p *micReopenPolicy) decideWithContext(now time.Time, source interactionSource, ctx micReopenContext) micReopenDecision {
	if ctx.ExistingReopen {
		// The stock logs "Existing mic reopen aborted by another request" and
		// then continues through the remaining gates.
		p.existingAborted = true
	}
	if !ctx.ListeningEnabled {
		return micReopenIgnoredListeningDisabled
	}
	if ctx.HoldToTalk {
		return micReopenIgnoredHoldToTalk
	}
	if source.isBargeIn() {
		return micReopenImmediateBargeIn
	}
	if ctx.ASRActive && !ctx.ASRStopping {
		return micReopenIgnoredASRActive
	}
	if p.maxRequests > 0 {
		cutoff := now.Add(-p.window)
		kept := p.requests[:0]
		for _, t := range p.requests {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		p.requests = kept
		if len(p.requests) >= p.maxRequests {
			return micReopenIgnoredRateLimit
		}
	}
	p.requests = append(p.requests, now)
	return micReopenAllowed
}

func (p *micReopenPolicy) reset() {
	p.requests = nil
	p.existingAborted = false
}

// reopenResolution is the stock MicReopenResolution enum.
type reopenResolution int

const (
	reopenDontReopen reopenResolution = 0
	reopenMagicMic   reopenResolution = 1
	reopenOther      reopenResolution = 2
)

func reopenResolutionForTrigger(trigger int) reopenResolution {
	if trigger == 3 {
		return reopenMagicMic
	}
	return reopenOther
}

// stockNoDynamicGain is the IEEE-754 single -1.0f sentinel used by
// ScheduleReopenMicrophone and StartListening.
const stockNoDynamicGain = -1.0

// stockFixedGainMultiplier reproduces the stock fixed-gain builder: the
// configured float is truncated to int and a multiplier is created only when
// the integer part is strictly greater than 1.
func stockFixedGainMultiplier(gain float64) (float64, bool) {
	n := int(gain)
	if n > 1 {
		return float64(n), true
	}
	return 1, false
}

func stockDynamicGainEnabled(gain float64, enabled bool) bool {
	return enabled && gain != stockNoDynamicGain
}

// hotwordDecision is the result of the stock hotword/loopback gate.
type hotwordDecision string

const (
	hotwordAccepted          hotwordDecision = "ACCEPTED"
	hotwordIgnoredLoopback   hotwordDecision = "IGNORED_LOOPBACK"
	hotwordIgnoredVoiceMatch hotwordDecision = "IGNORED_VOICE_MATCH"
	hotwordIgnoredASR        hotwordDecision = "IGNORED_ASR"
)

// hotwordLoopbackGuard mirrors the stock rules:
//   - Skip hotword processing in ASR state.
//   - Ignore detected hotword, because hotword was detected in loopback within ...
//   - Ignore detected hotword, because voice match verification fails.
//
// Ghidra 12.1.4 found the stock comparison uses 599,999 (≈600 ms if the clock
// is microseconds). The hotword score is logged, but no score-threshold
// comparison was found in that loopback path.
type hotwordLoopbackGuard struct {
	mu                 sync.Mutex
	loopbackWindow     time.Duration
	lastLoopback       time.Time
	lastLoopbackScore  float64
	asrActive          bool
	voiceMatchRequired bool
}

func newHotwordLoopbackGuard(loopbackWindow time.Duration) *hotwordLoopbackGuard {
	if loopbackWindow <= 0 {
		loopbackWindow = 600 * time.Millisecond
	}
	return &hotwordLoopbackGuard{loopbackWindow: loopbackWindow}
}

func (g *hotwordLoopbackGuard) noteLoopback(now time.Time, score float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastLoopback = now
	g.lastLoopbackScore = score
}

func (g *hotwordLoopbackGuard) setASRActive(active bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asrActive = active
}

func (g *hotwordLoopbackGuard) decide(now time.Time, score float64, voiceMatchOK bool) hotwordDecision {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.asrActive {
		return hotwordIgnoredASR
	}
	if !g.lastLoopback.IsZero() && now.Sub(g.lastLoopback) <= g.loopbackWindow {
		return hotwordIgnoredLoopback
	}
	if g.voiceMatchRequired && !voiceMatchOK {
		return hotwordIgnoredVoiceMatch
	}
	return hotwordAccepted
}

// dynamicGainConfig is the stock fixed/dynamic gain shape:
// fixed_gain, max_signal_gain, max_noise_gain and post_gain. The stock also
// has pcan_auto_gain_control_config and caches the last dynamic gain.
type dynamicGainConfig struct {
	FixedGain     float64
	MaxSignalGain float64
	MaxNoiseGain  float64
	PostGain      float64
	Enabled       bool
}

// dynamicGain is a small, explicit model of the observed gain policy. It is
// deliberately conservative: the fixed gain is the ceiling, a noisy block is
// limited by MaxNoiseGain, a clean block may use MaxSignalGain, and PostGain is
// applied last. This is an inference from the stock field names and log lines,
// not a recovered WebRTC AGC implementation.
type dynamicGain struct {
	cfg      dynamicGainConfig
	cached   float64
	hasCache bool
}

func (g *dynamicGain) compute(signalRMS, noiseRMS float64) float64 {
	fixed := g.cfg.FixedGain
	if fixed <= 0 {
		fixed = 1
	}
	if !g.cfg.Enabled {
		g.cached, g.hasCache = fixed, true
		return fixed
	}
	maxSignal := g.cfg.MaxSignalGain
	if maxSignal <= 0 {
		maxSignal = fixed
	}
	maxNoise := g.cfg.MaxNoiseGain
	if maxNoise <= 0 {
		maxNoise = fixed
	}
	allowed := maxSignal
	if noiseRMS > 0 && signalRMS <= noiseRMS*2 {
		allowed = maxNoise
	}
	gain := math.Min(fixed, allowed)
	if g.cfg.PostGain > 0 {
		gain *= g.cfg.PostGain
	}
	g.cached, g.hasCache = gain, true
	return gain
}

func (g *dynamicGain) cachedGain() (float64, bool) { return g.cached, g.hasCache }

func (g *dynamicGain) reset() { g.hasCache = false }

// arbitrationDecision is the stock device-arbitration decision enum:
// MUST_WIN NO_BIAS ON_DEVICE_ARBITER SHOULD_WIN.
type arbitrationDecision string

const (
	arbitrationMustWin         arbitrationDecision = "MUST_WIN"
	arbitrationShouldWin       arbitrationDecision = "SHOULD_WIN"
	arbitrationNoBias          arbitrationDecision = "NO_BIAS"
	arbitrationOnDeviceArbiter arbitrationDecision = "ON_DEVICE_ARBITER"
)

// duckingRole is the stock DuckingSenderEntry / DuckingReceiverEntry role.
type duckingRole string

const (
	duckingRoleNone     duckingRole = ""
	duckingRoleSender   duckingRole = "SENDER"
	duckingRoleReceiver duckingRole = "RECEIVER"
)

// Ghidra 12.1.4 recovered the sender/receiver entry delays and the default
// election timeout from FUN_004b4344 / FUN_004b4bb0 / FUN_004b3b58.
const (
	stockDuckingSenderEntryDelay   = 3
	stockDuckingReceiverEntryDelay = 2
	stockDuckingElectionTimeout    = 1_000_000
)

func duckingEntryDelay(role duckingRole) int {
	switch role {
	case duckingRoleSender:
		return stockDuckingSenderEntryDelay
	case duckingRoleReceiver:
		return stockDuckingReceiverEntryDelay
	default:
		return 0
	}
}

// duckingElection models the observed sender/receiver election. MUST_WIN and
// SHOULD_WIN make this device the ducking sender; otherwise it is a receiver.
// The stock messages "Tried to start ducking sender when we lost the election"
// and "Tried to start ducking when we won the election" are the guard rails.
type duckingElection struct {
	decision      arbitrationDecision
	role          duckingRole
	interactionID string
	active        bool
}

func (d *duckingElection) start(decision arbitrationDecision, interactionID string) (duckingRole, bool) {
	if interactionID == "" {
		return duckingRoleNone, false
	}
	role := duckingRoleReceiver
	if decision == arbitrationMustWin || decision == arbitrationShouldWin {
		role = duckingRoleSender
	}
	d.decision, d.role, d.interactionID, d.active = decision, role, interactionID, true
	return role, true
}

func (d *duckingElection) stop(interactionID string) bool {
	if !d.active || d.interactionID != interactionID {
		return false
	}
	d.active, d.role, d.interactionID = false, duckingRoleNone, ""
	return true
}

func (d *duckingElection) onRemoteDuck(uuid, interactionID string) (bool, string) {
	if uuid == "" || interactionID == "" {
		return false, "invalid_remote_duck"
	}
	if d.active && d.interactionID == interactionID && d.role == duckingRoleSender {
		return false, "already_sender"
	}
	d.role, d.interactionID, d.active = duckingRoleReceiver, interactionID, true
	return true, "receiver"
}

func (d *duckingElection) onRemoteUnduck(interactionID string) bool {
	if !d.active || d.interactionID != interactionID || d.role != duckingRoleReceiver {
		return false
	}
	d.active, d.role, d.interactionID = false, duckingRoleNone, ""
	return true
}

// ttsInterruptionReason is the observed TTS interruption path.
type ttsInterruptionReason string

const (
	ttsInterruptNone     ttsInterruptionReason = "NONE"
	ttsInterruptHotword  ttsInterruptionReason = "HOTWORD"
	ttsInterruptBargeIn  ttsInterruptionReason = "BARGE_IN"
	ttsInterruptStopword ttsInterruptionReason = "STOPWORD"
)

// ttsInterruptionPolicy mirrors the stock messages:
//   - TTS interrupted by hotword
//   - Barge in query validated! Interrupt TTS now.
//   - OnStopwordDetected: Stop TTS only.
//
// The stopword path is only enabled after the stopword pipeline is ready.
type ttsInterruptionPolicy struct {
	ttsPlaying      bool
	stopwordEnabled bool
}

func (p *ttsInterruptionPolicy) decide(source interactionSource, bargeInValidated bool) ttsInterruptionReason {
	if !p.ttsPlaying {
		return ttsInterruptNone
	}
	if source == sourceHotword {
		return ttsInterruptHotword
	}
	if source.isBargeIn() && bargeInValidated {
		return ttsInterruptBargeIn
	}
	if p.stopwordEnabled {
		return ttsInterruptStopword
	}
	return ttsInterruptNone
}
