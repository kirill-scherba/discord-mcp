package voicekit

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
	"strings"
	"time"

	"github.com/hraban/opus"
)

// edgeTTSPath is the edge-tts CLI inside the virtual environment.
const edgeTTSPath = "/home/kirill/edge-tts-venv/bin/edge-tts"

// defaultVoice is Baron's chosen edge-tts voice.
const defaultVoice = "ru-RU-DmitryNeural"

// openAITTSClient is a dedicated client for OpenAI speech synthesis.
var openAITTSClient = &http.Client{Timeout: 60 * time.Second}

// openAIVoices is a list of OpenAI voices to try, in order.
var openAIVoices = []string{"onyx", "alloy", "echo"}

// yandexVoices lists Yandex SpeechKit voices for Baron, in order.
var yandexVoices = []string{"kirill", "filipp", "anton", "marina"}

// TTSProvider returns the configured TTS provider:
// "edge", "openai", "yandex", "auto" (edge+openai fallback, default).
func TTSProvider() string {
	p := strings.ToLower(strings.TrimSpace(os.Getenv("TTS_PROVIDER")))
	switch p {
	case "edge", "openai", "yandex", "auto":
		return p
	default:
		return "auto"
	}
}

// TTSEdge synthesizes text with edge-tts and returns Opus frames.
func TTSEdge(text string) ([][]byte, error) {
	dir, err := os.MkdirTemp("", "voicekit-tts-*")
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
		return nil, fmt.Errorf("edge-tts: %v: %s", err, TrimWhitespace(string(out)))
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

// TTSYandex synthesizes text via the Yandex SpeechKit TTS API v3 and returns
// Opus frames. unsafe_mode is enabled only for texts > 250 chars (it splits
// the text into chunks and flattens intonation on short replies).
func TTSYandex(text string) ([][]byte, error) {
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
		if len(text) > 250 {
			body["unsafe_mode"] = true
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
			lastErr = fmt.Errorf("yandex tts HTTP %d: %s", resp.StatusCode, Truncate(string(jsonBody), 200))
			continue
		}

		// NDJSON: one JSON object per audio chunk. Collect all audioChunk.data.
		var mp3 []byte
		var streamErr error
		for _, line := range splitJSONObjects(string(jsonBody)) {
			var chunk struct {
				Result struct {
					AudioChunk struct {
						Data string `json:"data"`
					} `json:"audioChunk"`
				} `json:"result"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				streamErr = err
				continue
			}
			if chunk.Error != nil {
				streamErr = fmt.Errorf("yandex tts: %s", chunk.Error.Message)
				continue
			}
			if chunk.Result.AudioChunk.Data == "" {
				continue
			}
			part, err := base64.StdEncoding.DecodeString(chunk.Result.AudioChunk.Data)
			if err != nil {
				streamErr = fmt.Errorf("yandex tts: base64: %w", err)
				continue
			}
			mp3 = append(mp3, part...)
		}
		if streamErr != nil {
			lastErr = streamErr
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

// TTSOpenAI synthesizes text via the OpenAI Audio Speech API.
func TTSOpenAI(text string) ([][]byte, error) {
	key := os.Getenv("OPENAI_API_KEY")
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
			lastErr = fmt.Errorf("TTS HTTP %d: %s", resp.StatusCode, Truncate(string(mp3), 200))
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

// splitJSONObjects splits a stream of concatenated JSON objects (NDJSON).
func splitJSONObjects(s string) []string {
	var out []string
	depth := 0
	start := 0
	inStr := false
	escaped := false
	for i, r := range s {
		switch {
		case escaped:
			escaped = false
		case inStr && r == '\\':
			escaped = true
		case inStr && r == '"':
			inStr = false
		case !inStr && r == '"':
			inStr = true
		case !inStr && r == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case !inStr && r == '}':
			depth--
			if depth == 0 {
				out = append(out, s[start:i+1])
			}
		}
	}
	return out
}

// mp3ToOpusFrames converts mp3 bytes into 20ms Opus frames.
func mp3ToOpusFrames(mp3 []byte) ([][]byte, error) {
	dir, err := os.MkdirTemp("", "voicekit-tts-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	mp3Path := filepath.Join(dir, "voice.mp3")
	pcmPath := filepath.Join(dir, "voice.pcm")
	if err := os.WriteFile(mp3Path, mp3, 0o600); err != nil {
		return nil, err
	}

	cmd := exec.Command("ffmpeg", "-y",
		"-i", mp3Path,
		"-f", "s16le", "-ar", "48000", "-ac", "1",
		pcmPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %v: %s", err, TrimWhitespace(string(out)))
	}

	pcm, err := os.ReadFile(pcmPath)
	if err != nil {
		return nil, err
	}

	samples := len(pcm) / 2
	enc, err := opus.NewEncoder(SampleRate, Channels, opus.AppVoIP)
	if err != nil {
		return nil, err
	}

	var frames [][]byte
	buf := make([]byte, 10000)
	for i := 0; i+FrameSamples <= samples; i += FrameSamples {
		frame := make([]int16, FrameSamples)
		for j := 0; j < FrameSamples; j++ {
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
