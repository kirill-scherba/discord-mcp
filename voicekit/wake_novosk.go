//go:build !vosk

// Wake-word without the local Vosk recognizer (lightweight build for
// low-resource servers, e.g. the Russian VPS). With no server-side Vosk,
// the peer still supports the sleep/wake cycle: the native Android client
// runs its OWN local Vosk and wakes the server via a DataChannel command.
// The web client (browser) does not have local Vosk, so with this build it
// stays awake always (old behaviour) — acceptable for the lightweight
// server that serves the Android pocket mode.
package voicekit

import "fmt"

// WakeVosk is a no-op placeholder when built without the vosk tag.
type WakeVosk struct{}

// WakeTimeoutSec returns the active-mode inactivity timeout in seconds
// (env GOV_WAKE_TIMEOUT, default 45).
func WakeTimeoutSec() int {
	return 45
}

// NewWakeVosk returns an error so the peer stays awake (no wake-word).
func NewWakeVosk(word string) (*WakeVosk, error) {
	return nil, fmt.Errorf("vosk not built (use -tags vosk)")
}

// Feed is not used for the no-op build.
func (w *WakeVosk) Feed(frame []int16) bool { return false }

// Close is a no-op.
func (w *WakeVosk) Close() {}
