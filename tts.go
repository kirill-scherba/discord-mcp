// tts.go — text-to-speech with three providers:
//  1. edge-tts (Microsoft Edge cloud voices, free)
//  2. OpenAI Audio Speech API (tts-1)
//  3. Yandex SpeechKit TTS (API v3)
//
// All produce mp3; the shared pipeline converts to Opus frames for Discord
// voice playback: mp3 -> ffmpeg (s16le 48kHz mono PCM) -> Opus frames.
// Provider is selected at runtime via TTS_PROVIDER env var (see ttsProvider).
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/hraban/opus"
)

// edgeTTSPath is the edge-tts CLI inside the virtual environment.
const edgeTTSPath = "/home/kirill/edge-tts-venv/bin/edge-tts"

// defaultVoice is Baron's chosen voice (edge-tts, no accent, free).
const defaultVoice = "ru-RU-DmitryNeural"

// openAITTSClient is a dedicated client for OpenAI speech synthesis.
var openAITTSClient = &http.Client{Timeout: 60 * time.Second}

// openAIVoices is a list of OpenAI voices to try, in order. Baron uses a
// deep male voice (onyx) first, falling back to others if needed.
var openAIVoices = []string{"onyx", "alloy", "echo"}

// ttsEdge synthesizes text with edge-tts and returns Opus frames.
func ttsEdge(text string) ([][]byte, error) {
	dir, err := os.MkdirTemp("", "discord-tts-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	mp3Path := filepath.Join(dir, "voice.mp3")
	cmd := exec.Command(edgeTTSPath,
		"--voice", defaultVoice,
		"--text", text,
		"--write-media", mp3Path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("edge-tts: %v: %s", err, trimWhitespace(string(out)))
	}
	mp3, err := os.ReadFile(mp3Path)
	if err != nil {
		return nil, err
	}
	if len(mp3) == 0 {
		return nil, fmt.Errorf("edge-tts: empty audio")
	}
	return mp3ToOpusFrames(mp3)
}

// yandexVoices lists Yandex SpeechKit voices for Baron, in order. kirill is
// a male Russian voice; filipp is the premium male voice.
var yandexVoices = []string{"kirill", "filipp", "anton", "marina"}

// ttsYandex synthesizes text via the Yandex SpeechKit TTS API v3 and returns
// Opus frames. Uses YANDEX_AI_API_KEY (service account API key). The API
// returns JSON with base64-encoded audio in result.audioChunk.data.
func ttsYandex(text string) ([][]byte, error) {
	key := os.Getenv("YANDEX_AI_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("YANDEX_AI_API_KEY not set")
	}

	var lastErr error
	for _, voice := range yandexVoices {
		body := map[string]any{
			"text": text,
			"outputAudioSpec": map[string]any{
				"containerAudio": map[string]any{"containerAudioType": "MP3"},
			},
			"hints": []map[string]any{{"voice": voice}},
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}

		req, err := http.NewRequest(http.MethodPost,
			"https://tts.api.cloud.yandex.net/tts/v3/utteranceSynthesis",
			bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Api-Key "+key)
		req.Header.Set("Content-Type", "application/json")

		resp, err := openAITTSClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		jsonBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("yandex tts HTTP %d: %s", resp.StatusCode, truncate(string(jsonBody), 200))
			continue
		}

		var out struct {
			Result struct {
				AudioChunk struct {
					Data string `json:"data"`
				} `json:"audioChunk"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(jsonBody, &out); err != nil {
			lastErr = err
			continue
		}
		if out.Error != nil {
			lastErr = fmt.Errorf("yandex tts: %s", out.Error.Message)
			continue
		}
		mp3, err := base64.StdEncoding.DecodeString(out.Result.AudioChunk.Data)
		if err != nil {
			lastErr = fmt.Errorf("yandex tts: base64: %w", err)
			continue
		}
		if len(mp3) == 0 {
			lastErr = fmt.Errorf("yandex tts: empty audio")
			continue
		}
		frames, err := mp3ToOpusFrames(mp3)
		if err != nil {
			return nil, fmt.Errorf("yandex tts: %w", err)
		}
		return frames, nil
	}
	return nil, fmt.Errorf("yandex tts: all voices failed: %w", lastErr)
}

// ttsOpenAI synthesizes text via the OpenAI Audio Speech API and returns
// Opus frames. Uses OPENAI_API_KEY (same key as STT).
func ttsOpenAI(text string) ([][]byte, error) {	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY not set")
	}

	var lastErr error
	for _, voice := range openAIVoices {
		body := map[string]any{
			"model":           "tts-1",
			"input":           text,
			"voice":           voice,
			"response_format": "mp3",
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}

		req, err := http.NewRequest(http.MethodPost,
			"https://api.openai.com/v1/audio/speech", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")

		resp, err := openAITTSClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		mp3, readErr := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode != http.StatusOK || len(mp3) == 0 {
			lastErr = fmt.Errorf("TTS HTTP %d: %s", resp.StatusCode, truncate(string(mp3), 200))
			continue
		}
		frames, err := mp3ToOpusFrames(mp3)
		if err != nil {
			return nil, fmt.Errorf("openai tts: %w", err)
		}
		return frames, nil
	}
	return nil, fmt.Errorf("openai tts: all voices failed: %w", lastErr)
}

// mp3ToOpusFrames converts mp3 bytes into 20ms Opus frames for Discord voice.
func mp3ToOpusFrames(mp3 []byte) ([][]byte, error) {
	dir, err := os.MkdirTemp("", "discord-tts-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	mp3Path := filepath.Join(dir, "voice.mp3")
	pcmPath := filepath.Join(dir, "voice.pcm")
	if err := os.WriteFile(mp3Path, mp3, 0o600); err != nil {
		return nil, err
	}

	// ffmpeg -> raw PCM s16le 48kHz mono
	cmd := exec.Command("ffmpeg", "-y",
		"-i", mp3Path,
		"-f", "s16le", "-ar", "48000", "-ac", "1",
		pcmPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %v: %s", err, trimWhitespace(string(out)))
	}

	pcm, err := os.ReadFile(pcmPath)
	if err != nil {
		return nil, err
	}

	// PCM -> Opus frames (20 ms each)
	samples := len(pcm) / 2
	enc, err := opus.NewEncoder(sampleRate, channels, opus.AppVoIP)
	if err != nil {
		return nil, err
	}

	var frames [][]byte
	buf := make([]byte, 10000)
	for i := 0; i+frameSamples <= samples; i += frameSamples {
		frame := make([]int16, frameSamples)
		for j := 0; j < frameSamples; j++ {
			idx := (i + j) * 2
			frame[j] = int16(pcm[idx]) | int16(pcm[idx+1])<<8
		}
		n, err := enc.Encode(frame, buf)
		if err != nil {
			return nil, fmt.Errorf("opus encode: %w", err)
		}
		encoded := make([]byte, n)
		copy(encoded, buf[:n])
		frames = append(frames, encoded)
	}

	if len(frames) == 0 {
		return nil, fmt.Errorf("no audio frames generated")
	}
	return frames, nil
}

// truncate limits a string to n bytes for log messages.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
