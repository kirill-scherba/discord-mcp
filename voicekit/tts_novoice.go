// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build novoice

// Speech synthesis needs an Opus encoder, which is C. In a build without voice the functions still
// exist - so the shared helpers compile - and they say plainly that nothing will be spoken.

package voicekit

import "fmt"

// TTSProvider names the engine that would be used; empty in a voice-free build.
func TTSProvider() string { return "" }

func TTSEdge(text string) ([][]byte, error) {
	return nil, fmt.Errorf("TTS is not available in this build")
}

func TTSYandex(text string) ([][]byte, error) {
	return nil, fmt.Errorf("TTS is not available in this build")
}

func TTSOpenAI(text string) ([][]byte, error) {
	return nil, fmt.Errorf("TTS is not available in this build")
}
