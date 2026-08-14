# discord-mcp — Status

## Current (2026-08-14)

### Govorilka (active focus)
- WebRTC pocket voice works: browser + native Android app.
- Wake-word "Барон" via **local Vosk** (real STT, no false positives).
- Commands: молчи (sleep), стоп (interrupt), повтори (replay).
- Sound signals: two high beeps = wake, two low beeps = sleep.
- Noise gates + sleep timeout (45s) save Yandex STT money.
- Android: WebView removed, native VoiceService (BT SCO mic, auto-reconnect,
  audio dispose on destroy). No app changes needed for server-side fixes.

### Discord voice bot (stable, legacy path)
- DAVE voice bot joins the channel, hears speech, Baron replies.
- Not the current focus; still works.

## Milestones

- 2026-08-13: native Android audio pipeline fixed (OkHttp WS, BT SCO,
  audio routing, dispose); pocket mode reached.
- 2026-08-14: wake-word "Барон" — first DTW heuristics (rejected: false
  positives on similar phrases), then local Vosk (accepted).
- 2026-08-14: voice commands reworked (молчи = sleep, продолжаем removed).

## Known Issues

- Vosk model loaded per WebRTC connection; could be shared across peers.
- Vosk small RU model accuracy is limited; larger model option documented.
- DTW code (`voicekit/wake.go`) removed — Vosk replaced it.
