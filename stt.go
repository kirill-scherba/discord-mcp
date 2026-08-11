// stt.go — speech-to-text. Thin wrapper over voicekit (shared pipeline).
package main

import (
	"github.com/kirill-scherba/discord-mcp/voicekit"
)

// sttProvider returns the configured STT provider.
func sttProvider() string { return voicekit.STTProvider() }

// transcribe routes the WAV to the configured STT provider.
func transcribe(wav []byte) (string, error) { return voicekit.Transcribe(wav) }
