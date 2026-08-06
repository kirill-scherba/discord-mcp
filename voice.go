// voice.go — recording loop: Opus -> PCM -> VAD -> WAV -> STT -> brain -> TTS.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cartridge-gg/discordgo"
	"github.com/hraban/opus"
)

const (
	sampleRate    = 48000
	channels      = 1
	frameSamples  = 960 // 20 ms at 48 kHz
	silenceMS     = 400 // end utterance after this much silence
	maxUtteranceS = 30  // hard cap per utterance
	rmsThreshold  = 300 // voice activity threshold (int16)
)

// speaking flag prevents the bot from recording its own TTS playback.
func (b *bot) isSpeakingNow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ttsSpeaking
}

func (b *bot) setTTSSpeaking(v bool) {
	b.mu.Lock()
	b.ttsSpeaking = v
	b.mu.Unlock()
}

// recordingLoop reads Opus packets, decodes to PCM, detects speech with VAD,
// and on utterance end runs the whole STT -> brain -> TTS pipeline.
func (b *bot) recordingLoop(vc *discordgo.VoiceConnection, guildID, channelID string) {
	dec, err := opus.NewDecoder(sampleRate, channels)
	if err != nil {
		log.Printf("voice: opus decoder: %v", err)
		return
	}

	var pcm []int16
	var lastSpeech time.Time
	active := false

	log.Printf("voice: recording loop started in %s", channelID)

	for {
		// If the bot is currently speaking, drain packets and skip recording.
		if b.isSpeakingNow() {
			for len(vc.OpusRecv) > 0 {
				<-vc.OpusRecv
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}

		select {
		case p, ok := <-vc.OpusRecv:
			if !ok {
				log.Printf("voice: OpusRecv closed")
				return
			}
			frame := make([]int16, frameSamples)
			n, err := dec.Decode(p.Opus, frame)
			if err != nil || n == 0 {
				continue
			}
			frame = frame[:n]

			rms := rmsInt16(frame)
			if rms > rmsThreshold {
				pcm = append(pcm, frame...)
				lastSpeech = time.Now()
				if !active {
					active = true
				}
			} else if active {
				pcm = append(pcm, frame...)
			}

			if active && (time.Since(lastSpeech) > silenceMS*time.Millisecond ||
				len(pcm) > maxUtteranceS*sampleRate) {
				utterance := pcm
				pcm = nil
				active = false
				go b.processUtterance(vc, utterance)
			}

		case <-time.After(5 * time.Second):
			// idle: if there is an active utterance hanging, flush it
			if active && time.Since(lastSpeech) > silenceMS*time.Millisecond {
				utterance := pcm
				pcm = nil
				active = false
				go b.processUtterance(vc, utterance)
			}
		}
	}
}

// processUtterance runs STT -> brain -> TTS for a single recorded utterance.
// Serialized via processMu: while one utterance is being processed (including
// TTS playback), the next one waits. This prevents two replies from being
// synthesized and played at the same time.
func (b *bot) processUtterance(vc *discordgo.VoiceConnection, pcm []int16) {
	b.processMu.Lock()
	defer b.processMu.Unlock()

	if len(pcm) < sampleRate/4 { // ignore sub-250ms blips
		return
	}

	// Phase timing so the log shows exactly where the pipeline spends time.
	phaseStart := time.Now()
	step := func(name string) {
		log.Printf("voice: [phase] %s: %dms", name, time.Since(phaseStart).Milliseconds())
		phaseStart = time.Now()
	}

	wav := pcmToWAV(pcm)
	text, err := transcribe(wav)
	if err != nil {
		log.Printf("voice: STT failed: %v", err)
		return
	}
	step("stt")
	text = trimWhitespace(text)
	if text == "" {
		return
	}
	// Filter known Whisper hallucinations on silence/noise.
	if isGarbageSTT(text) {
		log.Printf("voice: STT garbage filtered: %q", text)
		return
	}
	log.Printf("voice: heard: %q", text)

	reply, err := brainAsk(text)
	if err != nil {
		log.Printf("voice: brain failed: %v", err)
		return
	}
	step("brain")
	reply = trimWhitespace(reply)
	if reply == "" {
		return
	}
	log.Printf("voice: reply: %q", reply)

	b.speak(vc, reply)
	step("tts-play")
}

// ttsProvider returns the configured TTS provider:
//   - "edge"   — Microsoft edge-tts only
//   - "openai" — OpenAI Audio Speech API only
//   - "yandex" — Yandex SpeechKit only
//   - "auto"   — edge-tts with OpenAI fallback (default)
//
// Controlled by the TTS_PROVIDER env var so switching during tests is a
// one-line change in the service unit + restart, no rebuild needed.
func ttsProvider() string {
	p := strings.ToLower(strings.TrimSpace(os.Getenv("TTS_PROVIDER")))
	switch p {
	case "edge", "openai", "yandex", "auto":
		return p
	default:
		return "auto"
	}
}

// speak synthesizes text via TTS and plays it into the voice channel.
// Provider is selected by TTS_PROVIDER: edge-tts, OpenAI, Yandex, or auto
// (edge-tts primary, OpenAI fallback on Microsoft throttling).
func (b *bot) speak(vc *discordgo.VoiceConnection, text string) {
	// Serialize playback: only one stream may write to OpusSend at a time.
	b.speakMu.Lock()
	defer b.speakMu.Unlock()

	if vc == nil || !vc.Ready {
		log.Printf("voice: not ready, cannot speak")
		return
	}

	var audio [][]byte
	var err error
	// synthStart measures only the TTS synthesis call (no playback).
	synthStart := time.Now()
	switch ttsProvider() {
	case "edge":
		audio, err = ttsEdge(text)
		if err != nil {
			log.Printf("voice: edge-tts failed: %v", err)
			return
		}
		log.Printf("voice: using edge-tts (Microsoft)")
	case "openai":
		audio, err = ttsOpenAI(text)
		if err != nil {
			log.Printf("voice: openai tts failed: %v", err)
			return
		}
		log.Printf("voice: using OpenAI TTS")
	case "yandex":
		audio, err = ttsYandex(text)
		if err != nil {
			log.Printf("voice: yandex tts failed: %v", err)
			return
		}
		log.Printf("voice: using Yandex TTS")
	default: // auto
		audio, err = ttsEdge(text)
		if err != nil {
			log.Printf("voice: edge-tts failed, falling back to OpenAI: %v", err)
			audio, err = ttsOpenAI(text)
			if err != nil {
				log.Printf("voice: TTS failed: %v", err)
				return
			}
			log.Printf("voice: using OpenAI TTS (fallback)")
		} else {
			log.Printf("voice: using edge-tts (Microsoft)")
		}
	}
	log.Printf("voice: [phase] tts-synth: %dms", time.Since(synthStart).Milliseconds())
	if len(audio) == 0 {
		log.Printf("voice: TTS produced no audio")
		return
	}

	b.setTTSSpeaking(true)
	defer b.setTTSSpeaking(false)

	if err := vc.Speaking(true); err != nil {
		log.Printf("voice: speaking(true): %v", err)
		return
	}
	defer vc.Speaking(false)

	for _, frame := range audio {
		select {
		case vc.OpusSend <- frame:
		case <-time.After(2 * time.Second):
			log.Printf("voice: opus send timeout, aborting playback")
			return
		}
	}
}

// pcmToWAV builds a 16-bit mono WAV file from PCM samples.
func pcmToWAV(pcm []int16) []byte {
	var buf bytes.Buffer
	dataLen := len(pcm) * 2
	// RIFF header
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(channels))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2))
	binary.Write(&buf, binary.LittleEndian, uint16(2)) // block align
	binary.Write(&buf, binary.LittleEndian, uint16(16)) // bits per sample
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(dataLen))
	for _, s := range pcm {
		binary.Write(&buf, binary.LittleEndian, s)
	}
	return buf.Bytes()
}

// rmsInt16 computes the RMS of a PCM frame.
func rmsInt16(frame []int16) float64 {
	if len(frame) == 0 {
		return 0
	}
	var sum float64
	for _, s := range frame {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(frame)))
}

func trimWhitespace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

// isGarbageSTT filters known Whisper hallucinations on silence/noise.
// Whisper sometimes invents subtitles, credits, or random phrases when
// the input is quiet — these get sent to Baron and pollute the session.
var garbageSTTPhrases = []string{
	"Редактор субтитров",
	"Корректор",
	"Субтитры",
	"Спасибо за просмотр",
	"Подписывайтесь на канал",
	"Thanks for watching",
	"Subtitles by",
}

func isGarbageSTT(text string) bool {
	for _, phrase := range garbageSTTPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// ensureVar keeps the os import used even if debug logging is disabled.
var _ = os.Getenv

var _ sync.Mutex // keep sync imported for future use

var _ = fmt.Sprintf
