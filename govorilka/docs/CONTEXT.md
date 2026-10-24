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
- Known open issue (deferred): short reply after an interrupt (pause 4-9s)
  is not heard by the native Android client; pause >15s or <3s plays fine.
  Deferred — needs a client-side test (logcat / test client).
- RULE from Kirill: the server must NEVER send extra traffic — no silence
  filler / comfort noise, ever.
- Committed: 40da04f (timestamp), 01539c5 (interrupt fixes). Not pushed.

## 2026-10-24 — "стало плохо без изменений": diagnosis and phase-0 fixes

Symptom: first connection took ages while the user shouted "Барон", then one
exchange worked and the connection seemed to reconnect forever.

Evidence collected:

- WebRTC ICE is FAST: `external IP override` -> `track received` in 0-1 s on
  every connection in the RU journal. The RKN/WebRTC-filtering hypothesis is
  NOT supported by the data.
- `stun.l.google.com:19302` does not answer from the RU VPS at all
  (`stun.sipnet.ru:3478` does). It does not matter while GOV_EXTERNAL_IP is
  set, but it is a trap for any future setup that relies on STUN.
- `govorilka.bmat.uk` (the app's default "hetzner" server, served from gt7-1)
  had a DEAD backend since 2026-09-26: its systemd user unit ran
  `ExecStart=/tmp/govorilka`, the file was gone, and the unit made 1 328 524
  restart attempts. nginx answered 502 on `/signal`; on 21 Oct the Android
  app hammered it 19 times in 4 minutes. That is the "долго-долго
  подключается" experience.
- The brain session had turned into a swamp: 3580 messages / 7.94 MB since
  16 Aug (model deepseek-v4-flash -> deepseek-flash), which made replies take
  8-15 s and sometimes made the model echo the whole transcript, which TTS
  then read aloud for a minute ("проходит одно сообщение и всё").
- The no-vosk RU build drops EVERY utterance while asleep, and the peer fell
  asleep ~5 s after every connection because `lastActive` was a zero
  time.Time: `time.Since(lastActive)` looked like 55 years of inactivity.

Fixes applied:

1. `lastActive: time.Now()` at peer creation — the peer now stays awake for
   the full GOV_WAKE_TIMEOUT (45 s) after connecting and after each utterance.
2. `GOV_DEBUG=1` gates per-packet logging (the old `rms < 5000` condition
   logged 50 lines/s of silence and produced a 942 MB journal).
3. `ice state` / `pc state` lines are logged on every connection.
4. Voice session rotation in `voicekit/brain.go`: state is
   `{id, created, turns}`; a session older than GOV_SESSION_MAX_HOURS
   (default 24) or longer than GOV_SESSION_MAX_TURNS (default 150) is
   retired. A legacy `{id}` file counts as rotten. A reply that contains
   `user[` or `ТЕХНИЧЕСКАЯ ИНФОРМАЦИЯ` (transcript echo) rotates the session
   and is replaced by a short apology instead of being spoken.
5. Replies are truncated to GOV_REPLY_MAX (default 500 chars) on a sentence
   boundary before TTS.
6. The gt7-1 unit no longer points at /tmp: `ExecStart=%h/go/bin/govorilka`,
   disabled as a cold standby (govorilka.bmat.uk is not the primary path;
   the phone uses govorilka-ru.bmat.uk).
7. State file path no longer depends on HOME: GOV_STATE_DIR, else
   $HOME/.config/mcp-gateway, else /var/lib/govorilka (the service has no
   HOME, so the old code wrote a RELATIVE path into /opt/.config/...).

Verification (2026-10-24): headless pion client (`/tmp/govtest`) connected in
0.16 s, ICE/PC states logged, sleep arrived at 45 s instead of 5 s, journal
went from ~40 000 lines/day to 21 lines per 5 minutes. A fresh brain session
answered in 1.8 s (second turn) against 8-15 s before.

Measured, not assumed: STUN from RU VPS, connect time, timeout, reply
latency. Run `go test ./voicekit/` for the session-rotation unit tests.
