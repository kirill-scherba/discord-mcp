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
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hraban/opus"
	"github.com/kirill-scherba/discord-mcp/voicekit"
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
}

// handleUtterance runs the voice pipeline for one utterance and sends the
// synthesized reply back over the output track.
func (p *govorilkaPeer) handleUtterance(pcm []int16) {
	if len(pcm) < 48000/4 { // ignore sub-250ms blips
		return
	}
	wav := voicekit.PCMToWAV(pcm)
	text, err := voicekit.Transcribe(wav)
	if err != nil {
		log.Printf("govorilka: STT: %v", err)
		return
	}
	text = voicekit.TrimWhitespace(text)
	if text == "" {
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
	p.mu.Unlock()
	if out == nil {
		return
	}
	for _, f := range frames {
		if _, err := out.Write(f); err != nil {
			log.Printf("govorilka: out write: %v", err)
			return
		}
	}
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

	// Create the output track that will carry the echo audio back.
	outTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: "audio/opus", ClockRate: 48000, Channels: 2},
		"echo", "govorilka")
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
		var lastSpeech time.Time
		active := false
		rtpBuf := make([]byte, 1600)
		pcmBuf := make([]int16, 960)
		for {
			n, _, err := track.Read(rtpBuf)
			if err != nil {
				log.Printf("govorilka: track read: %v", err)
				return
			}
			// RTP header is 12 bytes; the rest is the Opus payload.
			payload := rtpBuf[12:n]
			// Decode Opus payload -> PCM (20ms frame).
			decoded, derr := dec.Decode(payload, pcmBuf)
			if derr != nil || decoded == 0 {
				continue
			}
			rms := voicekit.RMSInt16(pcmBuf[:decoded])
			if rms > 800 {
				pcm = append(pcm, pcmBuf[:decoded]...)
				lastSpeech = time.Now()
				if !active {
					active = true
				}
			} else if active {
				pcm = append(pcm, pcmBuf[:decoded]...)
			}
			// Utterance end: 500ms silence or 28s cap.
			if active && (time.Since(lastSpeech) > 500*time.Millisecond ||
				len(pcm) > 28*48000) {
				utterance := pcm
				pcm = nil
				active = false
				go peer.handleUtterance(utterance)
			}
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
