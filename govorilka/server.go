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
}

// handleUtterance runs the voice pipeline for one utterance and sends the
// synthesized reply back over the output track. Serialized via sendMu:
// while a long reply is being sent, a new utterance waits (otherwise two
// goroutines would write RTP concurrently and corrupt playback).
func (p *govorilkaPeer) handleUtterance(pcm []int16) {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()

	log.Printf("govorilka: handleUtterance, %d samples (%.1f ms)", len(pcm), float64(len(pcm))/48.0)
	if len(pcm) < 48000/4 { // ignore sub-250ms blips
		return
	}
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
		return
	}
	log.Printf("govorilka: heard: %q", text)

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
		if err := http.ListenAndServe(govorilkaAddr, mux); err != nil {
			log.Printf("govorilka: server: %v", err)
		}
	}()
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

	// Create peer connection with Opus codec.
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		log.Printf("govorilka: pc: %v", err)
		return
	}
	defer pc.Close()

	peer := &govorilkaPeer{pc: pc}

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
					go peer.handleUtterance(utterance)
				} else {
					mu.Unlock()
				}
				timer.Reset(time.Hour)
			}
		}()

		for {
			rtpPacket, _, err := track.ReadRTP()
			if err != nil {
				log.Printf("govorilka: track read: %v", err)
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
			mu.Lock()
			if rms > 300 {
				pcm = append(pcm, pcmBuf[:decoded]...)
				if !active {
					active = true
				}
				// Reset the end-of-utterance timer only on speech, not on
				// silence packets — otherwise continuous silence keeps
				// restarting the 500ms timer forever.
				timer.Reset(500 * time.Millisecond)
			} else if active {
				pcm = append(pcm, pcmBuf[:decoded]...)
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
