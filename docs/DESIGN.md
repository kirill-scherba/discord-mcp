# discord-mcp — Design

## Components

```
┌─────────────────────────────────────────────────────────┐
│ discord-mcp (Go module)                                  │
│                                                         │
│  main.go ── server.NewMCPServer (stdio, JSON-RPC)        │
│   │        └── tools(): webhook send/read, bot status    │
│   └── go startBot()  (background, whole lifetime)        │
│        └── bot.go / voice.go                             │
│             Discord DAVE voice bot: joins channel,       │
│             STT -> brain (opencode) -> TTS               │
│                                                         │
│  voicekit/  — shared pipeline (STT/TTS/brain/audio)      │
│  govorilka/ — WebRTC voice prototype (separate service)  │
└─────────────────────────────────────────────────────────┘
```

## Discord voice bot (legacy path)

- `bot.go` — Discord session, DAVE encryption, channel join.
- `voice.go` — audio receive/send loop, utterance detection.
- `stt.go` / `stt_stream.go` / `tts.go` — thin wrappers over voicekit.
- Logs to `/tmp/discord-bot.log` (gateway sends child stderr to /dev/null).

## Govorilka (WebRTC pocket mode)

Separate systemd service (`govorilka`, :7790), shares `voicekit/`.

- Browser/Android -> WebRTC -> `/signal` -> PeerConnection (pion/webrtc).
- Opus decode -> PCM 48kHz.
- SLEEP: PCM -> local Vosk (wake word "Барон", real STT, free).
- ACTIVE: utterance -> Yandex STT -> brain -> Yandex TTS -> RTP reply.
- Sleep after 45s no speech or 5s noise.

See `govorilka/docs/DESIGN.md` for details.

## Why Vosk for the wake word

DTW on spectral features was tried first (2026-08-14) but cannot separate
"Барон" from similar phrases ("Лето в разгаре"). Vosk is a real local
recognizer: only actual speech triggers, ~5x realtime on CPU, no cloud
cost. Dependency: `vosk` python package + `vosk-model-small-ru-0.22`
(~88MB) — install steps in `govorilka/README.md`.
