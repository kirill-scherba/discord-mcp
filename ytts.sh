#!/usr/bin/env bash
# ytts.sh — Yandex SpeechKit TTS: text -> mp3
#
# Usage:
#   ./ytts.sh "Текст для озвучки" [voice] [output.mp3]
#   ./ytts.sh "Привет!"                  # voice=kirill, out=voice.mp3
#   ./ytts.sh "Привет!" filipp hi.mp3    # голос filipp, файл hi.mp3
#
# Requires: curl, python3, YANDEX_AI_API_KEY in environment or ~/.bashrc

set -euo pipefail

TEXT="${1:?usage: ytts.sh \"text\" [voice] [out.mp3]}"
VOICE="${2:-kirill}"
OUT="${3:-voice.mp3}"

# Load key from ~/.bashrc if not in env
if [[ -z "${YANDEX_AI_API_KEY:-}" ]]; then
  YANDEX_AI_API_KEY="$(grep '^export YANDEX_AI_API_KEY=' ~/.bashrc | cut -d= -f2)"
fi
[[ -n "$YANDEX_AI_API_KEY" ]] || { echo "YANDEX_AI_API_KEY not found" >&2; exit 1; }

# 1. POST request -> JSON with base64 audio
RESP="$(mktemp)"
curl -sS -X POST \
  -H "Authorization: Api-Key $YANDEX_AI_API_KEY" \
  -H "Content-Type: application/json" \
  -d "$(python3 -c '
import json, sys
print(json.dumps({
  "text": sys.argv[1],
  "outputAudioSpec": {"containerAudio": {"containerAudioType": "MP3"}},
  "hints": [{"voice": sys.argv[2]}]
}))' "$TEXT" "$VOICE")" \
  https://tts.api.cloud.yandex.net/tts/v3/utteranceSynthesis \
  --output "$RESP" -w "HTTP %{http_code}\n"

# 2. Extract audioChunk.data (base64) and decode to mp3
python3 -c '
import json, base64, sys
with open(sys.argv[1]) as f:
    data = json.load(f)
if "error" in data:
    print("API error:", data["error"]); sys.exit(1)
raw = base64.b64decode(data["result"]["audioChunk"]["data"])
with open(sys.argv[2], "wb") as f:
    f.write(raw)
print(f"OK: {len(raw)} bytes -> {sys.argv[2]}")' "$RESP" "$OUT"

rm -f "$RESP"
