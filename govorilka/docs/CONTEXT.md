# Govorilka — Memory Bank

Cross-session context for the Govorilka WebRTC voice prototype (pocket mode).

## What is Govorilka

WebRTC voice chat that replaces the phone in the pocket: the phone screen is
off, the mic is always on, and the server responds by voice — like Discord,
but native and self-hosted.

- Web page (RU): https://govorilka.bmat.uk (RU VPS reg.ru)
- RU server: systemd system service `govorilka`, port 7790, binary
  `/opt/govorilka`, host `194.58.95.91` (cv6416001.novalocal),
  EnvironmentFile `/etc/govorilka.env`
- Logs (RU): `govorilka/govlog-ru.sh -f` (uses SSH agent)
- Location: `govorilka/` in the `discord-mcp` repo
- Shared voice pipeline: `voicekit/` (STT/TTS/brain/audio helpers)

## Current State (2026-08-18)

Working end-to-end:

- WebRTC audio (mic -> server -> TTS reply) works in the browser and the
  native Android app.
- Voice commands: "молчи" (sleep), "стоп"/"хватит" (interrupt), "повтори"
  (replay), "барон" (wake / break-through).
- Sound signals: two beeps (ПИК-1 phrase accepted, ПИК-2 reply ready), low
  beep on "стоп".
- Noise gates: `GOV_ENERGY_GATE` (default 800), `GOV_VAD_THRESHOLD`
  (default 600). RU build is no-vosk: always listening, wake via client
  command or idle sleep (45s) + "Барон".

## Interrupt (barge-in) saga — resolved 2026-08-18

Symptom: "хватит" stops playback, but the next reply starts mid-way or a
short reply is not heard at all. After the 2nd/3rd interrupt it degraded;
the first interrupt after server restart was fine.

Root causes found and fixed:

1. **Race on seq/ts**: `go p.playClick(...)` ran the tone in parallel with
   the reply send loop; the tone could read p.seq/p.ts before the loop saved
   them and roll the counters BACK, so the next reply started with stale
   sequence numbers the client dropped as duplicates. Fixed: `playClick` is
   now a wrapper that takes `sendMu`; the internal `playClickLocked` is used
   for ПИК-1/ПИК-2 inside `handleUtterance` (sendMu already held).
2. **speakReply ignored interrupt**: a repeating long reply (100s) could not
   be stopped by "хватит". Fixed: `speakReply` checks `p.interrupt` in its
   send loop.
3. **Client jitter buffer desync after pause**: after an interrupt + TTS
   pause (4-9s) the client NetEq playout moves ahead of our continuing RTP
   timestamp, so the beginning of the next reply is dropped as late.
   Fixed: RTP **marker bit (M=1)** on the first packet of every reply,
   repeat and beep — standard "start of talkspurt" (RFC 3550), client
   resets its jitter buffer and plays from the start. seq/ts keep
   continuing (resetting per-utterance historically broke browser playback).

Deploy procedure (RU server):

```
cd govorilka && go build -o /tmp/govorilka_new .
export SSH_AUTH_SOCK=~/.ssh/agent/$(ls ~/.ssh/agent/ | head -1)
scp /tmp/govorilka_new root@194.58.95.91:/opt/govorilka.new
ssh root@194.58.95.91 'mv /opt/govorilka.new /opt/govorilka && chmod +x /opt/govorilka && systemctl restart govorilka'
```

## Known Issues / Next Steps

- Vosk model is loaded once per WebRTC connection (per client). Could be
  shared across peers to save RAM. (RU build is no-vosk by design.)
- Battery drain: mic always listens — goal "Экономия батареи:
  микрофон-детектор звука в говорилке" (sleep mic on low power).
- Android app is a separate build (WebView removed, native VoiceService);
  server-side changes do not require app updates.
- Uncommitted in working copy (pending final confirmation): playClick
  sendMu fix + speakReply barge-in + marker bit. Commit `40da04f`
  (timestamp) is local, not pushed.
