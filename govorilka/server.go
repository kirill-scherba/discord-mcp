// govorilka — prototype WebRTC voice page (no Discord).
//
// Self-contained mini-module inside the discord-mcp repository. It does not
// touch the discord-mcp code. Minimal proof-of-concept: a browser page
// connects over WebRTC, sends microphone audio as an Opus track; the server
// sends it straight back (echo) so the transport can be verified end-to-end.
//
// Real pipeline (STT -> brain -> TTS) plugs in later at the echo point.
package main

import (
	"encoding/json"
	"log"
	"math"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hraban/opus"
	"github.com/kirill-scherba/discord-mcp/voicekit"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// govorilkaAddr is where the prototype page + signaling listen.
const govorilkaAddr = ":7790"

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// govorilkaPeer keeps one WebRTC connection state.
type govorilkaPeer struct {
	mu       sync.Mutex
	pc       *webrtc.PeerConnection
	outTrack *webrtc.TrackLocalStaticRTP

	// sendMu serializes the full utterance pipeline + playback: if the user
	// speaks while a long reply is still being sent, a second goroutine
	// would write RTP packets concurrently and corrupt the stream.
	sendMu sync.Mutex

	// RTP sequence/timestamp counters persist across replies. Resetting
	// them per-utterance makes the browser treat later packets as stale
	// (seq wraps to 0) and silently drop them — only the first reply plays.
	seq uint16
	ts  uint32

	// Voice commands state (mirrors the Discord bot):
	lastReply string
	interrupt bool

	// replying is true while a reply is being played back (used by the
	// native client to skip STT during playback — its local Vosk handles
	// barge-in).
	replying bool

	// Wake-word (sleep/wake) state: the peer starts ASLEEP and only runs
	// the STT -> brain -> TTS pipeline after "Барон" is detected. An
	// inactivity timeout returns it to sleep, saving Yandex STT money.
	// The detector is the local Vosk recognizer (real speech recognition,
	// no false positives on similar-sounding phrases).
	wake        *voicekit.WakeVosk
	sleeping    bool
	wakeMu      sync.Mutex
	lastActive  time.Time

	// DataChannel for sleep/wake control commands (Android native client
	// mutes its mic track when the server sleeps; the client's local Vosk
	// sends a wake command over this channel to wake the server).
	dc *webrtc.DataChannel

	// isNative marks the native Android client (it opens its own
	// DataChannel "govorilka-ctl-client" and runs a local Vosk for
	// барон/хватит/молчи). Such clients do not need the server to listen
	// for barge-in during playback — saving STT on background noise.
	isNative bool
}

// handleUtterance runs the voice pipeline for one utterance and sends the
// synthesized reply back over the output track. Serialized via sendMu:
// while a long reply is being sent, a new utterance waits (otherwise two
// goroutines would write RTP concurrently and corrupt playback).
func (p *govorilkaPeer) handleUtterance(pcm []int16) {
	log.Printf("govorilka: handleUtterance, %d samples (%.1f ms)", len(pcm), float64(len(pcm))/48.0)
	if len(pcm) < 48000/4 { // ignore sub-250ms blips
		return
	}
	// Energy gate: background noise (fan, hum) during idle triggers VAD but
	// produces empty STT — which we pay for. If the utterance is quiet or
	// low-frequency hum (few zero crossings), drop it before sending to
	// Yandex. Thresholds come from GOV_ENERGY_GATE / GOV_ZCR_MIN env vars.
	zcr10ms := float64(voicekit.ZeroCrossings(pcm)) * 48000 / float64(len(pcm)) / 100
	if voicekit.IsNoise(pcm) {
		log.Printf("govorilka: noise utterance skipped (avgRms=%.0f zcr10ms=%.1f)", voicekit.AvgRMS(pcm), zcr10ms)
		return
	}
	log.Printf("govorilka: utterance passes gate (avgRms=%.0f zcr10ms=%.1f)", voicekit.AvgRMS(pcm), zcr10ms)
	wav := voicekit.PCMToWAV(pcm)
	// Save the last utterance for debugging (voice quality check).
	os.WriteFile("/tmp/govorilka_last.wav", wav, 0o644)
	text, err := voicekit.Transcribe(wav)
	if err != nil {
		log.Printf("govorilka: STT: %v", err)
		return
	}
	text = voicekit.TrimWhitespace(text)
	if text == "" {
		log.Printf("govorilka: STT empty (noise), %dms audio", len(pcm)/48)
		// Fast sleep: continuous noise (passed the energy gate but not
		// speech) — if no real speech was heard for > 5s, go back to sleep
		// right away instead of waiting for the full inactivity timeout.
		p.wakeMu.Lock()
		sleeping := p.sleeping
		p.wakeMu.Unlock()
		if !sleeping && time.Since(p.lastActive) > 5*time.Second {
			p.setSleeping(true)
			log.Printf("govorilka: noise for 5s -> sleeping (say %q to wake)", "Барон")
			// Sleep signal: two low beeps ("тук-тук").
			go func() {
				p.playClick(600, 16000)
				time.Sleep(120 * time.Millisecond)
				p.playClick(600, 16000)
			}()
			return
		}
		return
	}
	// Only REAL recognized speech extends the active window — noise that
	// passed the energy gate (loud hum, music) must NOT delay sleep.
	p.wakeMu.Lock()
	p.lastActive = time.Now()
	p.wakeMu.Unlock()
	log.Printf("govorilka: heard: %q", text)

	// Voice commands handled locally (mirror the Discord bot):
	cmd := voicekit.MatchCommand(text)
	switch cmd {
	// "стоп" interrupts playback without muting.
	case "стоп":
		log.Printf("govorilka: interrupt command: стоп")
		p.mu.Lock()
		p.interrupt = true
		p.mu.Unlock()
		// Acknowledge: short low beep so the user knows the command was
		// accepted (stopping playback is silent otherwise).
		go p.playClick(900, 12000)
		return
	case "повтори":
		p.mu.Lock()
		last := p.lastReply
		p.mu.Unlock()
		log.Printf("govorilka: повтори")
		if last == "" {
			p.speakReply("Мне пока нечего повторить.")
		} else {
			p.speakReply(last)
		}
		return
	// "молчи" goes to sleep: stop listening, wake up with "Барон".
	case "молчи":
		p.setSleeping(true)
		log.Printf("govorilka: молчи -> sleeping (say %q to wake)", "Барон")
		// Sleep signal: two low beeps ("тук-тук").
		go func() {
			p.playClick(600, 16000)
			time.Sleep(120 * time.Millisecond)
			p.playClick(600, 16000)
		}()
		return
	}

	// Tail rejection for ordinary speech: if a previous reply is still being
	// sent, this is a tail of the same phrase — drop it. Commands above are
	// NOT dropped (they must break through to interrupt/mute).
	if !p.sendMu.TryLock() {
		log.Printf("govorilka: tail dropped (previous still processing)")
		return
	}
	defer p.sendMu.Unlock()

	// For the native Android client: it runs a local Vosk and sends
	// barge-in ("хватит") via DataChannel, so the server does NOT need to
	// listen during playback. Drop everything while a reply is playing to
	// avoid paying STT for background noise on the client side.
	if p.isNative && p.replying {
		log.Printf("govorilka: native client, reply in progress -> drop (no STT)")
		return
	}

	// Clear any stale interrupt flag from a PREVIOUS reply BEFORE starting
	// brain/TTS. Do NOT clear it right before playback: for long replies the
	// TTS synthesis takes seconds, and a "хватит" heard during synthesis
	// must survive to interrupt the playback that follows.
	p.mu.Lock()
	p.interrupt = false
	p.mu.Unlock()

	// ПИК-1 (низкий): фраза принята и отправлена на обработку.
	p.playClick(1200, 6000)

	reply, err := voicekit.BrainAsk(text)
	if err != nil {
		log.Printf("govorilka: brain: %v", err)
		return
	}
	reply = voicekit.TrimWhitespace(reply)
	if reply == "" {
		return
	}
	log.Printf("govorilka: reply: %q", reply)
	p.mu.Lock()
	p.lastReply = reply
	p.mu.Unlock()

	// Synthesize reply audio and send it back over the WebRTC track.
	provider := voicekit.TTSProvider()
	var frames [][]byte
	switch provider {
	case "edge":
		frames, err = voicekit.TTSEdge(reply)
	case "openai":
		frames, err = voicekit.TTSOpenAI(reply)
	default: // yandex, auto
		frames, err = voicekit.TTSYandex(reply)
	}
	if err != nil {
		log.Printf("govorilka: TTS: %v", err)
		return
	}
	log.Printf("govorilka: sending %d opus frames", len(frames))

	// ПИК-2 (высокий): ответ готов, сейчас озвучится.
	p.playClick(1800, 6000)

	p.mu.Lock()
	out := p.outTrack
	seq := p.seq
	ts := p.ts
	p.mu.Unlock()
	if out == nil {
		return
	}
	// TrackLocalStaticRTP.Write expects a full RTP packet (header+payload).
	// Wrap each Opus frame in an RTP packet with SSRC/seq/timestamp.
	// seq/ts persist across replies (see struct comment). Pace at real-time
	// speed (20ms per frame) so the browser jitter buffer is not flooded.
	ssrc := uint32(4242)
	frameDur := 20 * time.Millisecond
	p.mu.Lock()
	p.replying = true
	p.mu.Unlock()
	for _, f := range frames {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    111, // Opus
				SequenceNumber: seq,
				Timestamp:      ts,
				SSRC:           ssrc,
			},
			Payload: f,
		}
		// Barge-in: "стоп"/"молчи" while replying stops playback at once.
		p.mu.Lock()
		interrupted := p.interrupt
		p.mu.Unlock()
		if interrupted {
			log.Printf("govorilka: interrupted, stopping playback")
			break
		}
		if err := out.WriteRTP(pkt); err != nil {
			log.Printf("govorilka: out write: %v", err)
			return
		}
		seq++
		ts += 960 // 20ms at 48kHz
		time.Sleep(frameDur)
	}
	p.mu.Lock()
	p.seq = seq
	p.ts = ts
	p.interrupt = false
	p.replying = false
	p.mu.Unlock()
}

// speakReply synthesizes a short phrase (command confirmation) and sends it
// to the browser. Unlike handleUtterance, it does not run the STT/brain
// pipeline — it speaks the given text directly.
func (p *govorilkaPeer) speakReply(text string) {
	frames, err := voicekit.TTSYandex(text)
	if err != nil {
		log.Printf("govorilka: reply TTS: %v", err)
		return
	}
	log.Printf("govorilka: reply-voice: %q (%d frames)", text, len(frames))

	p.mu.Lock()
	p.interrupt = false
	out := p.outTrack
	seq := p.seq
	ts := p.ts
	p.mu.Unlock()
	if out == nil {
		return
	}
	ssrc := uint32(4242)
	for _, f := range frames {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    111,
				SequenceNumber: seq,
				Timestamp:      ts,
				SSRC:           ssrc,
			},
			Payload: f,
		}
		if err := out.WriteRTP(pkt); err != nil {
			return
		}
		seq++
		ts += 960
		time.Sleep(20 * time.Millisecond)
	}
	p.mu.Lock()
	p.seq = seq
	p.ts = ts
	p.mu.Unlock()
}

// playClick sends a short feedback tone ("tack") to the browser so the user
// knows what is happening: low (1200Hz) = phrase accepted, high (1800Hz) =
// reply ready. Uses the same outTrack and RTP wrapping as replies.
func (p *govorilkaPeer) playClick(freq, amp float64) {
	p.mu.Lock()
	out := p.outTrack
	p.mu.Unlock()
	if out == nil {
		return
	}

	// 60ms tone with fast decay.
	const clickDur = 60 * time.Millisecond
	const sr = 48000.0
	n := int(sr * clickDur.Seconds())
	clickPCM := make([]int16, n)
	for i := 0; i < n; i++ {
		t := float64(i) / sr
		env := math.Exp(-t * 80)
		clickPCM[i] = int16(amp * math.Sin(2*math.Pi*freq*t) * env)
	}

	enc, err := opus.NewEncoder(48000, 1, opus.AppVoIP)
	if err != nil {
		return
	}
	var frames [][]byte
	buf := make([]byte, 10000)
	for i := 0; i+960 <= n; i += 960 {
		outN, err := enc.Encode(clickPCM[i:i+960], buf)
		if err != nil {
			return
		}
		f := make([]byte, outN)
		copy(f, buf[:outN])
		frames = append(frames, f)
	}
	if len(frames) == 0 {
		return
	}

	p.mu.Lock()
	seq := p.seq
	ts := p.ts
	p.mu.Unlock()
	ssrc := uint32(4242)
	for _, f := range frames {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    111,
				SequenceNumber: seq,
				Timestamp:      ts,
				SSRC:           ssrc,
			},
			Payload: f,
		}
		if err := out.WriteRTP(pkt); err != nil {
			return
		}
		seq++
		ts += 960
		time.Sleep(20 * time.Millisecond)
	}
	p.mu.Lock()
	p.seq = seq
	p.ts = ts
	p.mu.Unlock()
}

// startGovorilka launches the prototype server (non-blocking).
func startGovorilka() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})
	mux.HandleFunc("/signal", govorilkaSignal)
	go func() {
		log.Printf("govorilka: prototype on %s", govorilkaAddr)
		log.Printf("govorilka: VAD threshold=%.0f energy gate=%.0f zcr min=%.0f (GOV_* env)",
			voicekit.VADThreshold(), voicekit.EnergyGate(), voicekit.ZCRMin())
		if err := http.ListenAndServe(govorilkaAddr, mux); err != nil {
			log.Printf("govorilka: server: %v", err)
		}
	}()
}

// sendControl sends a JSON control command to the client over the
// DataChannel (sleep/wake state). The native Android client mutes its mic
// track when told to sleep, and unmutes on wake.
func (p *govorilkaPeer) sendControl(obj map[string]any) {
	if p.dc == nil || p.dc.ReadyState() != webrtc.DataChannelStateOpen {
		return
	}
	if b, err := json.Marshal(obj); err == nil {
		p.dc.SendText(string(b))
	}
}

// setSleeping transitions the peer to/from sleep and notifies the client
// so it can mute/unmute its mic track.
func (p *govorilkaPeer) setSleeping(sleeping bool) {
	p.wakeMu.Lock()
	p.sleeping = sleeping
	if sleeping {
		p.lastActive = time.Time{} // force timeout logic to re-arm
	} else {
		// Waking up: reset the inactivity timer so the peer does NOT
		// immediately fall back asleep (lastActive was zeroed on sleep).
		p.lastActive = time.Now()
	}
	p.wakeMu.Unlock()
	if sleeping {
		p.sendControl(map[string]any{"state": "sleep"})
	} else {
		p.sendControl(map[string]any{"state": "awake"})
	}
}

// govorilkaSignal is the WebSocket signaling endpoint: browser sends
// offer/candidate, receives answer/candidate.
func govorilkaSignal(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("govorilka: upgrade: %v", err)
		return
	}
	defer ws.Close()

	// Create peer connection with Opus codec. STUN server is configurable
	// via GOV_STUN (default Google). If the server is behind NAT with a
	// known public IP (e.g. reg.ru VPS: 192.168.x.x inside, 194.58.x.x
	// outside), set GOV_EXTERNAL_IP so WebRTC advertises it directly
	// instead of relying on STUN (which may be blocked/unavailable).
	stun := os.Getenv("GOV_STUN")
	if stun == "" {
		stun = "stun:stun.l.google.com:19302"
	}
	cfg := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{stun}}},
	}
	var pc *webrtc.PeerConnection
	var pcErr error
	if ext := os.Getenv("GOV_EXTERNAL_IP"); ext != "" {
		s := webrtc.SettingEngine{}
		s.SetNAT1To1IPs([]string{ext}, webrtc.ICECandidateTypeHost)
		api := webrtc.NewAPI(webrtc.WithSettingEngine(s))
		pc, pcErr = api.NewPeerConnection(cfg)
		log.Printf("govorilka: external IP override: %s", ext)
	} else {
		pc, pcErr = webrtc.NewPeerConnection(cfg)
	}
	if pcErr != nil {
		log.Printf("govorilka: pc: %v", pcErr)
		return
	}
	// NOTE: pc is NOT closed when the signaling ws closes. The browser closes
	// the ws after WebRTC connects (it is only needed for the handshake), so
	// closing pc here would kill the media. pc is closed when the mic track
	// ends (client leaves) — see OnTrack EOF.

	peer := &govorilkaPeer{pc: pc}

	// Wake-word detector: the peer starts asleep and waits for "Барон",
	// recognized by the local Vosk speech recognizer (real STT, no false
	// positives). If Vosk cannot start the peer stays awake (old behaviour).
	if det, err := voicekit.NewWakeVosk("Барон"); err == nil {
		peer.wake = det
		peer.sleeping = true
		log.Printf("govorilka: wake-word mode ON (vosk), timeout=%ds",
			voicekit.WakeTimeoutSec())
	} else {
		log.Printf("govorilka: wake-word disabled (%v), always listening", err)
	}

	// Control DataChannel: server -> client sleep/wake state, client ->
	// server wake command (native Android client with local Vosk).
	if dc, err := pc.CreateDataChannel("govorilka-ctl", nil); err == nil {
		peer.dc = dc
		// When the channel opens, tell the client the CURRENT state — the
		// peer may already be asleep (new client connecting to a sleeping
		// server must mute its mic immediately, not only on state changes).
		dc.OnOpen(func() {
			peer.wakeMu.Lock()
			sleeping := peer.sleeping
			peer.wakeMu.Unlock()
			state := "awake"
			if sleeping {
				state = "sleep"
			}
			peer.sendControl(map[string]any{"state": state})
			log.Printf("govorilka: sent initial state to client: %s", state)
		})
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			var cmd struct {
				Cmd string `json:"cmd"`
			}
			if err := json.Unmarshal(msg.Data, &cmd); err != nil {
				return
			}
			switch cmd.Cmd {
			case "wake":
				peer.wakeMu.Lock()
				sleeping := peer.sleeping
				peer.wakeMu.Unlock()
				if sleeping {
					peer.setSleeping(false)
					log.Printf("govorilka: wake command from client -> listening")
					// Awake signal: two quick high beeps.
					go func() {
						peer.playClick(1200, 16000)
						time.Sleep(120 * time.Millisecond)
						peer.playClick(1800, 16000)
					}()
				}
			case "sleep":
				peer.wakeMu.Lock()
				sleeping := peer.sleeping
				peer.wakeMu.Unlock()
				if !sleeping {
					peer.setSleeping(true)
					log.Printf("govorilka: sleep command from client -> sleeping")
					// Sleep signal: two low beeps.
					go func() {
						peer.playClick(600, 16000)
						time.Sleep(120 * time.Millisecond)
						peer.playClick(600, 16000)
					}()
				}
			case "stop":
				peer.mu.Lock()
				peer.interrupt = true
				peer.mu.Unlock()
				log.Printf("govorilka: stop command from client -> interrupt")
				go peer.playClick(900, 12000)
			}
		})
	} else {
		log.Printf("govorilka: control datachannel: %v", err)
	}

	// Detect the native Android client: it opens its own control channel
	// "govorilka-ctl-client" (for барон/хватит/молчи via local Vosk).
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() == "govorilka-ctl-client" {
			peer.wakeMu.Lock()
			peer.isNative = true
			peer.wakeMu.Unlock()
			log.Printf("govorilka: native android client detected")
		}
	})

	// Output track carries the synthesized reply back to the browser.
	// The earlier acoustic loop was caused by HTML auto-playing the received
	// stream (srcObject = own mic); that was removed, so the track is safe.
	outTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: "audio/opus", ClockRate: 48000, Channels: 2},
		"reply", "govorilka")
	if err != nil {
		log.Printf("govorilka: out track: %v", err)
		return
	}
	peer.outTrack = outTrack
	if _, err := pc.AddTrack(outTrack); err != nil {
		log.Printf("govorilka: add track: %v", err)
		return
	}

	// Handle incoming microphone track: decode Opus, detect utterance end,
	// run STT -> brain -> TTS, send the reply back over the output track.
	pc.OnTrack(func(track *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
		log.Printf("govorilka: track received: %s", track.Codec().MimeType)
		dec, err := opus.NewDecoder(48000, 1)
		if err != nil {
			log.Printf("govorilka: opus decoder: %v", err)
			return
		}
		var pcm []int16
		active := false
		pcmBuf := make([]int16, 960)
		// Pre-roll ring buffer: keep the last ~300ms of audio so that when
		// speech starts (VAD trip) the beginning of the first word is not
		// lost. Without it, "Барон" said from silence is captured as "-арон"
		// and the wake detector misses it; "Привет Барон" works because the
		// VAD is already active when "Барон" begins.
		preRoll := make([]int16, 0, 480*6)
		var mu sync.Mutex
		pktCount := 0

		// WebRTC browsers do NOT send silence packets — the track only carries
		// actual speech. A timer goroutine finalizes the utterance 500ms after
		// the last speech packet (track.Read blocks, so the main loop can't
		// notice silence itself).
		timer := time.NewTimer(time.Hour)
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		go func() {
			for {
				<-timer.C
				mu.Lock()
				if active {
					utterance := pcm
					pcm = nil
					active = false
					mu.Unlock()
					log.Printf("govorilka: timer fired, utterance %d samples", len(utterance))
					// While asleep, do NOT feed utterances to STT.
					peer.wakeMu.Lock()
					sleeping := peer.sleeping
					peer.wakeMu.Unlock()
					if sleeping {
						// While asleep we do NOT process utterances: either
						// the server-side Vosk listens in the main loop, or
						// (build without vosk) the client wakes us via a
						// DataChannel command. Either way no STT cost here.
					} else {
						go peer.handleUtterance(utterance)
					}
				} else {
					mu.Unlock()
				}
				timer.Reset(time.Hour)
			}
		}()

		// Sleep watchdog: return to sleep after inactivity (no utterances
		// for GOV_WAKE_TIMEOUT seconds while listening).
		go func() {
			for {
				time.Sleep(5 * time.Second)
				peer.wakeMu.Lock()
				sleeping := peer.sleeping
				lastActive := peer.lastActive
				peer.wakeMu.Unlock()
				if !sleeping {
					if time.Since(lastActive) > time.Duration(voicekit.WakeTimeoutSec())*time.Second {
						peer.setSleeping(true)
						log.Printf("govorilka: idle timeout -> sleeping (say %q to wake)", "Барон")
						// Sleep signal: two low beeps.
						go func() {
							peer.playClick(600, 16000)
							time.Sleep(120 * time.Millisecond)
							peer.playClick(600, 16000)
						}()
						continue
					}
				}
			}
		}()

		for {
			rtpPacket, _, err := track.ReadRTP()
			if err != nil {
				log.Printf("govorilka: track read: %v", err)
				// Client left — close the peer connection (it is no longer
				// tied to the signaling ws, which closed after handshake).
				pc.Close()
				return
			}
			pktCount++
			if pktCount <= 3 {
				pl := rtpPacket.Payload
				if len(pl) > 5 {
					log.Printf("govorilka: pkt%d payload6=%02x %02x %02x %02x %02x %02x payloadLen=%d",
						pktCount, pl[0], pl[1], pl[2], pl[3], pl[4], pl[5], len(pl))
				}
			}
			// rtpPacket.Payload is the raw Opus data (pion parsed the RTP
			// header, including any extension).
			payload := rtpPacket.Payload
			// Decode Opus payload -> PCM (20ms frame).
			decoded, derr := dec.Decode(payload, pcmBuf)
			if derr != nil || decoded == 0 {
				continue
			}
			rms := voicekit.RMSInt16(pcmBuf[:decoded])
			if pktCount%50 == 1 || rms < 5000 {
				log.Printf("govorilka: pkt decoded=%d rms=%.0f active=%v", decoded, rms, active)
			}
			// While asleep, feed every decoded frame to the Vosk wake-word
			// recognizer (it listens continuously in the stream).
			peer.wakeMu.Lock()
			sleeping := peer.sleeping
			wake := peer.wake
			peer.wakeMu.Unlock()
			if sleeping && wake != nil {
				if wake.Feed(pcmBuf[:decoded]) {
					peer.setSleeping(false)
					log.Printf("govorilka: wake word detected -> listening")
					// Awake signal: two quick high beeps.
					go func() {
						peer.playClick(1200, 16000)
						time.Sleep(120 * time.Millisecond)
						peer.playClick(1800, 16000)
					}()
				}
				continue
			}
			mu.Lock()
			if rms > voicekit.VADThreshold() {
				// Speech starts: prepend the pre-roll (recent quiet audio)
				// so the onset of the first word is captured.
				if !active {
					pcm = append(pcm, preRoll...)
					preRoll = preRoll[:0]
					active = true
				}
				pcm = append(pcm, pcmBuf[:decoded]...)
				// Reset the end-of-utterance timer only on speech, not on
				// silence packets — otherwise continuous silence keeps
				// restarting the 500ms timer forever.
				timer.Reset(500 * time.Millisecond)
			} else if active {
				pcm = append(pcm, pcmBuf[:decoded]...)
			} else {
				// Idle: keep the rolling pre-roll buffer (last ~300ms).
				preRoll = append(preRoll, pcmBuf[:decoded]...)
				if len(preRoll) > 480*6 {
					preRoll = append([]int16(nil), preRoll[len(preRoll)-480*6:]...)
				}
			}
			mu.Unlock()
		}
	})

	// ICE candidates from browser -> server.
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		msg, _ := json.Marshal(map[string]any{"signal": "candidate", "data": c.ToJSON()})
		ws.WriteMessage(websocket.TextMessage, msg)
	})

	// Signaling loop: read offer/answer/candidate from browser.
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			log.Printf("govorilka: signal read: %v", err)
			return
		}
		var msg struct {
			Signal string          `json:"signal"`
			Data   json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Signal {
		case "offer":
			var offer webrtc.SessionDescription
			json.Unmarshal(msg.Data, &offer)
			if err := pc.SetRemoteDescription(offer); err != nil {
				log.Printf("govorilka: set remote: %v", err)
				return
			}
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				log.Printf("govorilka: answer: %v", err)
				return
			}
			if err := pc.SetLocalDescription(answer); err != nil {
				log.Printf("govorilka: set local: %v", err)
				return
			}
			resp, _ := json.Marshal(map[string]any{"signal": "answer", "data": answer})
			ws.WriteMessage(websocket.TextMessage, resp)
		case "candidate":
			var cand webrtc.ICECandidateInit
			json.Unmarshal(msg.Data, &cand)
			pc.AddICECandidate(cand)
		}
	}
}
