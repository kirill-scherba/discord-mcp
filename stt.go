// stt.go — speech-to-text with two providers:
//  1. OpenAI Audio API (whisper) — default
//  2. Yandex SpeechKit STT (synchronous recognition v1)
//
// Provider is selected at runtime via STT_PROVIDER env var (see sttProvider).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
)
// sttProvider returns the configured STT provider:
//   - "whisper"      — OpenAI whisper (default)
//   - "yandex"       — Yandex SpeechKit synchronous
//   - "yandex-stream"— Yandex SpeechKit streaming (no phrase splitting)
//
// Controlled by the STT_PROVIDER env var.
func sttProvider() string {
	p := strings.ToLower(strings.TrimSpace(os.Getenv("STT_PROVIDER")))
	switch p {
	case "whisper", "yandex", "yandex-stream":
		return p
	default:
		return "whisper"
	}
}

// transcribe routes the WAV to the configured STT provider.
func transcribe(wav []byte) (string, error) {
	if sttProvider() == "yandex" {
		return sttYandex(wav)
	}
	return sttWhisper(wav)
}

// sttWhisper sends a WAV file to the OpenAI transcription endpoint and
// returns the recognized Russian text.
func sttWhisper(wav []byte) (string, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return "", fmt.Errorf("OPENAI_API_KEY not set")
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	fw, err := mw.CreateFormFile("file", "voice.wav")
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(wav); err != nil {
		return "", err
	}
	mw.WriteField("model", "whisper-1")
	mw.WriteField("language", "ru")
	// Prompt gives Whisper context and dramatically reduces hallucinations
	// on silence/noise (e.g. "Редактор субтитров А.Синецкая...").
	mw.WriteField("prompt", "Голосовой чат. Говорит пользователь.")
	mw.Close()

	req, err := http.NewRequest(http.MethodPost,
		"https://api.openai.com/v1/audio/transcriptions", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("STT HTTP %d: %s", resp.StatusCode, body)
	}

	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	return out.Text, nil
}

// sttYandex sends WAV to the Yandex SpeechKit synchronous recognition API
// and returns the recognized Russian text. The API expects raw LPCM (no WAV
// header), mono. We downsample 48 kHz -> 16 kHz (3:1) before sending: Yandex
// recognizes 16 kHz fine, and this triples the allowed audio length under the
// 1 MB request limit (~32s instead of ~10.9s).
func sttYandex(wav []byte) (string, error) {
	key := os.Getenv("YANDEX_AI_API_KEY")
	if key == "" {
		return "", fmt.Errorf("YANDEX_AI_API_KEY not set")
	}

	// Strip the WAV header (44 bytes for standard PCM) to get raw LPCM.
	lpcm := wav
	if len(wav) >= 44 && bytes.Equal(wav[:4], []byte("RIFF")) {
		lpcm = wav[44:]
	}
	// Downsample 48000 -> 16000: take every 3rd int16 sample.
	// (Linear interpolation between samples would be smoother, but 3:1
	// decimation is fine for speech recognition.)
	s16 := lpcm
	if len(lpcm) > 0 {
		n := len(lpcm) / 2
		out := make([]byte, 0, (n/3)*2+2)
		for i := 0; i < n; i += 3 {
			out = append(out, lpcm[i*2], lpcm[i*2+1])
		}
		s16 = out
	}

	u := "https://stt.api.cloud.yandex.net/speech/v1/stt:recognize?" + url.Values{
		"lang":             {"ru-RU"},
		"topic":            {"general"},
		"format":           {"lpcm"},
		"sampleRateHertz":  {"16000"},
		"rawResults":       {"true"},
		"profanityFilter":  {"false"},
	}.Encode()

	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(s16))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Api-Key "+key)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("yandex STT HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var out struct {
		Result string `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.Error != "" {
		return "", fmt.Errorf("yandex STT: %s", out.Error)
	}
	return strings.TrimSpace(out.Result), nil
}
