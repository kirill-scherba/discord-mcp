# Govorilka — WebRTC voice prototype (pocket mode)

Voice chat that works with the phone in the pocket: screen off, mic always
on, server answers by voice. Replaces Discord voice for this use case.

- Web page: https://govorilka.bmat.uk
- Server: systemd user service `govorilka` on port 7790
- Logs: `./govlog.sh` or `journalctl --user -u govorilka`

## Requirements

- Go (build)
- ffmpeg (STT/TTS audio conversion)
- **Vosk** + Russian model (wake-word "Барон" — local speech recognition)
- Yandex Cloud keys for STT/TTS (`YANDEX_AI_API_KEY`)

## Vosk installation (wake-word dependency)

The server uses the local **Vosk** speech recognizer to listen for the wake
word "Барон" while asleep. Vosk is NOT a cloud service — it runs on the
server, so wake-word detection costs nothing.

```bash
# 1. Install the vosk python package (Arch: python-pip first)
sudo pacman -S python-pip
python3 -m pip install --break-system-packages vosk

# 2. Download the Russian model (~88 MB)
mkdir -p ~/models
cd ~/models
curl -LO https://alphacephei.com/vosk/models/vosk-model-small-ru-0.22.zip
unzip vosk-model-small-ru-0.22.zip
rm vosk-model-small-ru-0.22.zip
```

The model path and the python site-packages path are hardcoded in
`voicekit/wake_vosk.go` (`voskScript` and `NewWakeVosk`). If you install
to a different location, update those constants.

To switch to a bigger/more accurate model (e.g. `vosk-model-ru-0.42`):
download it, and update the model path inside `voskScript`.

## Build & run

```bash
cd <repo>/govorilka
go build -o /tmp/govorilka .
# systemd user service (govorilka.service) starts /tmp/govorilka
```

## Configuration

See `docs/DESIGN.md` — env vars in `~/.config/mcp-gateway/env`
(`GOV_*`, `STT_PROVIDER`, `TTS_PROVIDER`).

## Docs

- `docs/CONTEXT.md` — current state, known issues
- `docs/DESIGN.md` — architecture, sleep/wake, components
