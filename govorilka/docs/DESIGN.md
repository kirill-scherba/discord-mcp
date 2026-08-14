# Govorilka — Design

## Architecture

```
Browser / Android App                    Server (govorilka)
┌──────────────┐   WebRTC (Opus)    ┌─────────────────────────────┐
│ mic → RTP    │ ──────────────────▶│ /signal (WebSocket handshake)│
│              │                    │  PeerConnection (pion/webrtc)│
│ TTS ← RTP    │ ◀──────────────────│  opus decode → PCM 48kHz     │
└──────────────┘                    │                             │
                                    │ SLEEP: PCM → Vosk (wake word)│
                                    │ ACTIVE: utterance → Yandex   │
                                    │   STT → brain → Yandex TTS  │
                                    └─────────────────────────────┘
```

## Wake-word ("Барон") via Vosk

The server runs **local Vosk speech recognition** (real STT) while asleep:

- **SLEEP mode**: every decoded 20ms frame is fed to a Vosk subprocess via
  stdin. Vosk recognizes speech locally (no cloud calls, no money spent).
  When it emits the wake word "барон", the peer switches to ACTIVE.
- **ACTIVE mode**: utterances go through the normal pipeline — Yandex STT
  (or configured provider) → brain (opencode) → Yandex TTS → RTP reply.
- **Back to sleep**: after `GOV_WAKE_TIMEOUT` (45s) of no recognized
  speech, or after 5s of continuous noise (STT empty), the peer sleeps
  again.

Why Vosk and not DTW heuristics? DTW on spectral features cannot reliably
separate "Барон" from similar-sounding phrases ("Лето в разгаре", "Да
здравствуйте"). Vosk is a real recognizer: it triggers only on actual
speech, runs ~5x faster than real time on CPU, and costs nothing.

## Voice commands (recognized by Yandex STT in ACTIVE mode)

- `молчи` / `замолчи` — go to sleep (stop listening; wake with "Барон")
- `стоп` / `остановись` / `хватит` — interrupt playback (stay awake)
- `повтори` — replay the last reply

## Sleep/Wake state machine

```
        ┌─────────┐  "Барон" (Vosk)   ┌─────────┐
        │  SLEEP  │ ────────────────▶ │  ACTIVE │
        │ (no STT)│                    │(STT/brain│
        └─────────┘ ◀─────────────────└─────────┘
             45s no speech OR 5s noise
```

## Key components

- `govorilka/server.go` — WebRTC signaling, RTP loop, sleep/wake, commands
- `voicekit/wake_vosk.go` — Vosk subprocess wrapper (feed PCM, detect word)
- `voicekit/` — shared STT/TTS/brain/audio pipeline
- `govorilka/index.html` — browser client (WebRTC, watchdog reconnect)
- `govorilka/android/` — native Android client (VoiceService)
- `govorilka/govlog.sh` — log viewer (events, wake/sleep, heard/reply)

## Configuration (env, in ~/.config/mcp-gateway/env)

| Var | Default | Meaning |
|---|---|---|
| `GOV_VAD_THRESHOLD` | 600 | per-frame RMS to start an utterance |
| `GOV_ENERGY_GATE` | 800 | utterance RMS below this = noise, drop before STT |
| `GOV_ZCR_MIN` | 0 | zero-crossing filter (0 = disabled) |
| `GOV_WAKE_TIMEOUT` | 45 | seconds of inactivity before sleep |
| `STT_PROVIDER` | yandex | STT provider (yandex, whisper) |
| `TTS_PROVIDER` | yandex | TTS provider (yandex, edge, openai) |
