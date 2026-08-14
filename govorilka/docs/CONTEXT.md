# Govorilka — Memory Bank

Cross-session context for the Govorilka WebRTC voice prototype (pocket mode).

## What is Govorilka

WebRTC voice chat that replaces the phone in the pocket: the phone screen is
off, the mic is always on, and the server responds by voice — like Discord,
but native and self-hosted.

- Web page: https://govorilka.bmat.uk
- Server: systemd user service `govorilka`, port 7790, binary `/tmp/govorilka`
- Logs: `journalctl --user -u govorilka` or `govorilka/govlog.sh`
- Location: `govorilka/` in the `discord-mcp` repo
- Shared voice pipeline: `voicekit/` (STT/TTS/brain/audio helpers)

## Current State (2026-08-14)

Working end-to-end:

- WebRTC audio (mic -> server -> TTS reply) works in the browser and the
  native Android app.
- Wake-word "Барон" via **local Vosk speech recognition** (real STT, not
  DTW heuristics). The server starts ASLEEP and wakes on "Барон".
- Voice commands: "молчи" (sleep), "стоп" (interrupt), "повтори" (replay).
- Sound signals: two high beeps = woke up, two low beeps = fell asleep.
- Noise gates: `GOV_ENERGY_GATE` (default 800) rejects quiet noise before
  paying for Yandex STT; `GOV_VAD_THRESHOLD` (default 600) per-frame VAD.
- Sleep: after `GOV_WAKE_TIMEOUT` (default 45s) of no recognized speech,
  or after 5s of continuous noise, the peer goes back to sleep.

## Known Issues / Next Steps

- Vosk model is loaded once per WebRTC connection (per client). Could be
  shared across peers to save RAM.
- Wake-word recognition quality depends on the Vosk small RU model; a
  larger model (vosk-model-ru-0.42) improves accuracy at the cost of CPU.
- Android app is a separate build (WebView removed, native VoiceService);
  server-side changes do not require app updates.
