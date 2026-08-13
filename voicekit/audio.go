package voicekit

import (
	"bytes"
	"encoding/binary"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// httpClient shared by webhook/STT/TTS calls.
var httpClient = &http.Client{Timeout: 60 * time.Second}

// SampleRate is the audio sample rate used across the pipeline.
const SampleRate = 48000

// Channels is the number of audio channels (mono).
const Channels = 1

// FrameSamples is samples per 20 ms Opus frame at 48 kHz.
const FrameSamples = 960

// PCMToWAV builds a 16-bit mono WAV file from PCM samples.
func PCMToWAV(pcm []int16) []byte {
	var buf bytes.Buffer
	dataLen := len(pcm) * 2
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(Channels))
	binary.Write(&buf, binary.LittleEndian, uint32(SampleRate))
	binary.Write(&buf, binary.LittleEndian, uint32(SampleRate*2))
	binary.Write(&buf, binary.LittleEndian, uint16(2))  // block align
	binary.Write(&buf, binary.LittleEndian, uint16(16)) // bits per sample
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(dataLen))
	for _, s := range pcm {
		binary.Write(&buf, binary.LittleEndian, s)
	}
	return buf.Bytes()
}

// RMSInt16 computes the RMS of a PCM frame.
func RMSInt16(frame []int16) float64 {
	if len(frame) == 0 {
		return 0
	}
	var sum float64
	for _, s := range frame {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(frame)))
}

// AvgRMS computes the average RMS over a whole buffer (energy gate).
func AvgRMS(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, s := range pcm {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(pcm)))
}

// ZeroCrossings counts how many times the signal crosses zero. Low-frequency
// hum/fan noise has few crossings; speech (consonants) crosses much more
// often. Used to reject steady noise that still passes the energy gate.
func ZeroCrossings(pcm []int16) int {
	zc := 0
	for i := 1; i < len(pcm); i++ {
		if (pcm[i-1] < 0) != (pcm[i] < 0) {
			zc++
		}
	}
	return zc
}

// envFloat reads a float env var with a default.
func envFloat(name string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// VADThreshold is the per-frame RMS above which a packet counts as speech.
func VADThreshold() float64 { return envFloat("GOV_VAD_THRESHOLD", 1000) }

// EnergyGate is the average RMS below which a whole utterance is dropped
// before STT (background noise triggers VAD but is not speech).
func EnergyGate() float64 { return envFloat("GOV_ENERGY_GATE", 800) }

// ZCRMin is the minimum zero-crossings-per-10ms for an utterance to be
// speech-like; 0 disables the ZCR filter (old behaviour).
func ZCRMin() float64 { return envFloat("GOV_ZCR_MIN", 120) }

// IsNoise reports whether the utterance looks like steady background noise
// rather than speech: quiet (below the energy gate) or too few zero
// crossings for its length (low-frequency hum).
func IsNoise(pcm []int16) bool {
	if len(pcm) == 0 {
		return true
	}
	if AvgRMS(pcm) < EnergyGate() {
		return true
	}
	zcrMin := ZCRMin()
	if zcrMin <= 0 {
		return false
	}
	// Zero crossings per 10 ms window.
	dur10ms := float64(len(pcm)) / float64(SampleRate) * 100
	if dur10ms <= 0 {
		return false
	}
	zcr := float64(ZeroCrossings(pcm)) / dur10ms
	if zcr < zcrMin {
		return true
	}
	return false
}

// TrimWhitespace trims leading/trailing whitespace.
func TrimWhitespace(s string) string {
	return strings.TrimSpace(s)
}

// Truncate limits a string to n bytes for log messages.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// IsGarbageSTT filters known Whisper hallucinations on silence/noise.
var garbageSTTPhrases = []string{
	"Редактор субтитров",
	"Корректор",
	"Субтитры",
	"Спасибо за просмотр",
	"Подписывайтесь на канал",
	"Thanks for watching",
	"Subtitles by",
}

// IsGarbageSTT reports whether the text is a known STT hallucination.
func IsGarbageSTT(text string) bool {
	for _, phrase := range garbageSTTPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}
