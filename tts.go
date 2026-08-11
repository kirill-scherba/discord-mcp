// tts.go — text-to-speech. Thin wrapper over voicekit (shared pipeline).
package main

import (
	"github.com/kirill-scherba/discord-mcp/voicekit"
)

// ttsProvider returns the configured TTS provider.
func ttsProvider() string { return voicekit.TTSProvider() }

// ttsEdge synthesizes text with edge-tts.
func ttsEdge(text string) ([][]byte, error) { return voicekit.TTSEdge(text) }

// ttsYandex synthesizes text via Yandex SpeechKit.
func ttsYandex(text string) ([][]byte, error) { return voicekit.TTSYandex(text) }

// ttsOpenAI synthesizes text via OpenAI.
func ttsOpenAI(text string) ([][]byte, error) { return voicekit.TTSOpenAI(text) }
