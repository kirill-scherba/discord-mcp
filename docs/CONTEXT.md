# discord-mcp — Memory Bank

Cross-session context for the discord-mcp module (Discord MCP server + DAVE
voice bot + Govorilka WebRTC voice prototype).

## What is discord-mcp

Go module that provides:
1. **Discord MCP server** — webhook messaging tools + DAVE-encrypted voice
   bot (Baron) that auto-joins a voice channel and listens for speech.
2. **Govorilka** — a WebRTC voice prototype (pocket mode: phone in pocket,
   screen off, mic always on) that replaces Discord voice for that use
   case. Lives in `govorilka/`, shares the `voicekit/` pipeline.

Served to the MCP gateway via stdio (JSON-RPC 2.0). Logs go to
`/tmp/discord-bot.log`.

## Key facts

- Bot token + voice channel: via env (see `bot.go`, `voice.go`).
- Voice pipeline: STT -> brain (opencode) -> TTS, shared in `voicekit/`.
- Govorilka web page: https://govorilka.bmat.uk, systemd user service
  `govorilka`, port 7790.
- Current focus (2026-08-14): Govorilka wake-word "Барон" via local Vosk.

## Docs

- `govorilka/docs/CONTEXT.md` — Govorilka current state
- `govorilka/docs/DESIGN.md` — Govorilka architecture
- `govorilka/README.md` — Govorilka build + Vosk install
