// helpers.go — shared helpers for the discord-mcp main package, delegating
// to voicekit where the logic lives now.
package main

import (
	"github.com/kirill-scherba/discord-mcp/voicekit"
)

// truncate limits a string to n bytes for log messages.
func truncate(s string, n int) string { return voicekit.Truncate(s, n) }

// trimWhitespace trims leading/trailing whitespace.
func trimWhitespace(s string) string { return voicekit.TrimWhitespace(s) }

// isGarbageSTT filters known Whisper hallucinations.
func isGarbageSTT(text string) bool { return voicekit.IsGarbageSTT(text) }

// pcmToWAV builds a WAV file from PCM samples.
func pcmToWAV(pcm []int16) []byte { return voicekit.PCMToWAV(pcm) }

// rmsInt16 computes RMS of a frame.
func rmsInt16(frame []int16) float64 { return voicekit.RMSInt16(frame) }

// avgRMS computes average RMS of a buffer.
func avgRMS(pcm []int16) float64 { return voicekit.AvgRMS(pcm) }
