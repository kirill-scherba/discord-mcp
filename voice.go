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
	silenceMS     = 500 // end utterance after this much silence
	maxUtteranceS = 30  // hard cap per utterance
	rmsThreshold  = 800 // voice activity threshold (int16); high enough to
	// ignore background noise, low enough to catch normal speech
	// minSpeechMS is the minimum continuous speech before an utterance is
	// considered real (filters out noise blips that would otherwise be sent
	// to STT and billed).
	minSpeechMS = 600
	// minTailSpeechMS: utterances shorter than this that started while the
	// bot was busy are treated as tails and dropped; longer ones are kept.
	minTailSpeechMS = 2000
)

// speaking flag prevents the bot from recording its own TTS playback.
func (b *bot) isSpeakingNow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ttsSpeaking
}

// isBusy reports whether an utterance is being processed (STT/brain/TTS).
func (b *bot) isBusy() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.busy
}

// setBusy marks the pipeline as busy (utterance in flight). While busy the
// recording loop discards all incoming audio.
func (b *bot) setBusy(v bool) {
	b.mu.Lock()
	b.busy = v
	b.mu.Unlock()
}

// wasBusyAt reports whether the bot was busy (speaking or processing a
// previous utterance) at the given time. Used to reject utterance tails that
// were recorded while the bot was replying.
func (b *bot) wasBusyAt(t time.Time) bool {
	if b.isSpeakingNow() {
		return true
	}
	// If processMu is locked right now, a previous utterance is still being
	// processed — a phrase that started recently is likely its tail.
	if !b.processMu.TryLock() {
		return true
	}
	b.processMu.Unlock()
	return false
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
	var speechStart time.Time
	active := false

	log.Printf("voice: recording loop started in %s", channelID)

	for {
		// While an utterance is being processed (STT/brain/TTS), discard all
		// incoming audio: it is either the tail of the phrase in flight or
		// speech that cannot interrupt the bot anyway.
		if b.isBusy() {
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
					speechStart = time.Now()
				}
			} else if active {
				pcm = append(pcm, frame...)
			}

			if active && (time.Since(lastSpeech) > silenceMS*time.Millisecond ||
				len(pcm) > maxUtteranceS*sampleRate) {
				utterance := pcm
				pcm = nil
				active = false
				// Mark busy BEFORE launching: from this instant all incoming
				// audio (the phrase tail, anything said during processing) is
				// discarded until the reply has been played.
				b.setBusy(true)
				go b.processUtterance(vc, utterance, speechStart)
			}

		case <-time.After(5 * time.Second):
			// idle: if there is an active utterance hanging, flush it
			if active && time.Since(lastSpeech) > silenceMS*time.Millisecond {
				utterance := pcm
				pcm = nil
				active = false
				b.setBusy(true)
				go b.processUtterance(vc, utterance, speechStart)
			}
		}
	}
}

// recordingLoopStream is the streaming STT variant (STT_PROVIDER=yandex-stream).
// Instead of waiting for a silence timer, PCM chunks are pushed to the Yandex
// streaming API while the user speaks; the server signals endOfUtterance when
// the phrase is complete, so pauses inside a phrase no longer split it.
func (b *bot) recordingLoopStream(vc *discordgo.VoiceConnection, guildID, channelID string) {
	dec, err := opus.NewDecoder(sampleRate, channels)
	if err != nil {
		log.Printf("voice: opus decoder: %v", err)
		return
	}

	var s *sttStream
	var chunk []int16
	chunkN := 0
	active := false

	log.Printf("voice: streaming recording loop started in %s", channelID)

	// results feeds final/eou events from the stream reader goroutine.
	results := make(chan string, 4)
	streamDone := make(chan struct{})

	// readStream drains server responses; on eou sends the final text.
	readStream := func(st *sttStream) {
		defer close(streamDone)
		for {
			text, ended, rerr := st.recvResult()
			if rerr != nil {
				log.Printf("voice: stream recv: %v", rerr)
				return
			}
			if ended {
				results <- text
				return
			}
		}
	}

	for {
		// While the bot speaks, pause streaming entirely: close any active
		// stream and drain incoming packets. This avoids concurrent UDP
		// read/write on the voice socket (opusSender vs opusReceiver) which
		// deadlocks DAVE encryption. The user simply cannot talk while the
		// bot is replying; after playback ends, streaming resumes.
		if b.isSpeakingNow() {
			if s != nil {
				log.Printf("voice: pausing stream (bot speaking)")
				s.close()
				s = nil
				chunk = nil
				chunkN = 0
				active = false
			}
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
				if s != nil {
					s.close()
				}
				return
			}
			frame := make([]int16, frameSamples)
			n, derr := dec.Decode(p.Opus, frame)
			if derr != nil || n == 0 {
				continue
			}
			frame = frame[:n]

			rms := rmsInt16(frame)
			if rms > rmsThreshold {
				// Speech started: open the stream once.
				if s == nil {
					s, err = startYandexStream()
					if err != nil {
						log.Printf("voice: stream start failed: %v", err)
						return
					}
					go readStream(s)
				}
				active = true
				chunk = append(chunk, frame...)
				chunkN++
				// Push every streamChunkFrames frames (~100ms).
				if chunkN >= streamChunkFrames {
					if serr := s.sendPCM(chunk); serr != nil {
						log.Printf("voice: stream send: %v", serr)
						s.close()
						s = nil
						chunk = nil
						chunkN = 0
						active = false
					}
					chunk = nil
					chunkN = 0
				}
			} else if active {
				// Silence while streaming: still send it so the server sees
				// the pause and eventually emits eou_update.
				chunk = append(chunk, frame...)
				chunkN++
				if chunkN >= streamChunkFrames {
					if serr := s.sendPCM(chunk); serr != nil {
						log.Printf("voice: stream send: %v", serr)
						s.close()
						s = nil
						chunk = nil
						chunkN = 0
						active = false
					}
					chunk = nil
					chunkN = 0
				}
			}

		case text := <-results:
			// End of utterance from the server.
			text = trimWhitespace(text)
			if text != "" && !isGarbageSTT(text) {
				log.Printf("voice: [stream] eou: %q", text)
				go b.processStreamText(vc, text)
			} else {
				log.Printf("voice: [stream] eou empty/garbage, skipped")
			}
			if s != nil {
				s.close()
				s = nil
			}
			chunk = nil
			chunkN = 0
			active = false

		case <-time.After(streamSilenceInterval):
			// No Discord packets for a while: the user stopped talking.
			// Tell the server about the silence so it emits eou_update for
			// the completed utterance, instead of hanging until timeout.
			if active && s != nil {
				if serr := s.sendSilence(streamSilenceInterval.Milliseconds()); serr != nil {
					log.Printf("voice: stream silence: %v", serr)
					s.close()
					s = nil
					chunk = nil
					chunkN = 0
					active = false
				}
			}
		}
	}
}

// processStreamText runs brain -> TTS for a text already recognized by the
// streaming STT (no STT step, it happened during recording).
func (b *bot) processStreamText(vc *discordgo.VoiceConnection, text string) {
	log.Printf("voice: processStreamText start: %q", text)
	b.processMu.Lock()
	log.Printf("voice: processStreamText got processMu")
	defer b.processMu.Unlock()

	phaseStart := time.Now()
	step := func(name string) {
		log.Printf("voice: [phase] %s: %dms", name, time.Since(phaseStart).Milliseconds())
		phaseStart = time.Now()
	}

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

// processUtterance runs STT -> brain -> TTS for a single recorded utterance.
// Serialized via processMu: while one utterance is being processed (including
// TTS playback), the next one waits. This prevents two replies from being
// synthesized and played at the same time.
func (b *bot) processUtterance(vc *discordgo.VoiceConnection, pcm []int16, speechStart time.Time) {
	// busy was set before this goroutine started; clear it on every exit
	// (tail/noise drops, errors, or after the reply has been played).
	defer b.setBusy(false)

	// Tail rejection: if the bot was speaking (or processing) when this
	// utterance started, and the utterance is SHORT, it is the tail of a
	// phrase already split by the silence timer — drop it. Long utterances
	// (>= minTailSpeechMS) are kept even if they started while the bot was
	// busy: the user is clearly saying a new, full phrase.
	if b.wasBusyAt(speechStart) &&
		time.Since(speechStart) < minTailSpeechMS*time.Millisecond {
		log.Printf("voice: tail dropped (bot was busy at speech start)")
		return
	}

	// Check BEFORE taking processMu: while waiting for the mutex, elapsed
	// time keeps growing and would let short noise pass the duration check.
	speechMS := time.Since(speechStart).Milliseconds()
	if speechMS < minSpeechMS {
		log.Printf("voice: short utterance (%dms) skipped", speechMS)
		return
	}

	// Ignore tiny PCM buffers (noise blips) regardless of elapsed time.
	if len(pcm) < sampleRate/4 { // ignore sub-250ms blips
		log.Printf("voice: tiny utterance (%d bytes) skipped", len(pcm)*2)
		return
	}

	// Energy gate: even if VAD fired, reject very quiet buffers (breath,
	// mic rustle) that would just be billed as empty STT.
	if avgRMS(pcm) < rmsThreshold*0.6 {
		log.Printf("voice: low-energy utterance skipped (avgRms=%.0f)", avgRMS(pcm))
		return
	}

	// Feedback click: the phrase passed all filters and is being processed.
	// Play a short "tack" (low tone) so the user knows their voice was accepted.
	b.playClick(vc, 1200, 6000)

	b.processMu.Lock()
	defer b.processMu.Unlock()

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
		log.Printf("voice: STT empty (noise), %dms audio, skipping", len(pcm)/sampleRate*1000)
		return
	}
	// Filter known Whisper hallucinations on silence/noise.
	if isGarbageSTT(text) {
		log.Printf("voice: STT garbage filtered: %q", text)
		return
	}
	log.Printf("voice: heard: %q", text)
	log.Printf("voice: heard pcm=%dms", len(pcm)/sampleRate*1000)

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

	// Second click (higher tone): the reply is ready, about to be spoken.
	// Splits the wait into two known phases so the user knows it's working.
	b.playClick(vc, 1800, 6000)

	b.speak(vc, reply)
	step("tts-play")
}

// playClick plays a short feedback tone ("tack") into the voice channel.
// It tells the user their phrase was accepted and is being processed.
// freq/amp select the tone: first click is lower (accepted), second is
// higher (reply ready) so the two phases are distinguishable.
func (b *bot) playClick(vc *discordgo.VoiceConnection, freq, amp float64) {
	if vc == nil || !vc.Ready {
		return
	}

	// 60ms tone with fast decay — a soft "tack", not annoying.
	const clickDur = 60 * time.Millisecond
	n := int(sampleRate * clickDur.Seconds())
	clickPCM := make([]int16, n)
	for i := 0; i < n; i++ {
		t := float64(i) / sampleRate
		env := math.Exp(-t * 80) // quick exponential decay
		clickPCM[i] = int16(amp * math.Sin(2*math.Pi*freq*t) * env)
	}

	// Encode PCM -> Opus frames (20ms each).
	enc, err := opus.NewEncoder(sampleRate, channels, opus.AppVoIP)
	if err != nil {
		log.Printf("voice: click encoder: %v", err)
		return
	}
	var frames [][]byte
	buf := make([]byte, 10000)
	for i := 0; i+frameSamples <= n; i += frameSamples {
		out, err := enc.Encode(clickPCM[i:i+frameSamples], buf)
		if err != nil {
			return
		}
		f := make([]byte, out)
		copy(f, buf[:out])
		frames = append(frames, f)
	}
	if len(frames) == 0 {
		return
	}

	// Send the tone without claiming "speaking" for long.
	if err := vc.Speaking(true); err != nil {
		return
	}
	defer vc.Speaking(false)
	for _, fr := range frames {
		select {
		case vc.OpusSend <- fr:
		case <-time.After(time.Second):
			return
		}
	}
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

	log.Printf("voice: speak start")

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

// avgRMS computes the average RMS over a whole buffer (used as an energy gate
// before sending to STT).
func avgRMS(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, s := range pcm {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(pcm)))
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
