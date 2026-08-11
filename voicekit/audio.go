package voicekit

import (
	"bytes"
	"encoding/binary"
	"math"
	"net/http"
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
