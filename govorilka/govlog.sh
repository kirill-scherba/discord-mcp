#!/bin/bash
# govlog — удобный просмотр лога говорилки: только ключевые события.
#
# Usage:
#   govlog            — последние события
#   govlog -f         — следить (tail -f)
#   govlog -n 50      — последние 50 событий
#   govlog -p         — БЕЗ "pkt decoded" (только события, без громкости)
#   govlog -qa        — только вопрос → ответ (heard/reply)
#   govlog -w         — только режим сна/бодрствования (wake word события)
#
# Показывает: heard (что распознано), reply (ответ Барона),
# sending (сколько фреймов отправлено), ошибки, длительность фраз,
# wake-word детект ("Барон" -> listening) и переходы sleep/wake.
N="${2:-30}"
TAIL=""
NO_PKT=0
WAKE_ONLY=0

case "$1" in
  -f|-F) TAIL="-f" ;;
  -p)    NO_PKT=1 ;;
  -pf|-fp) TAIL="-f"; NO_PKT=1 ;;
  -qa)   # вопрос → ответ, только heard и reply
    journalctl --user -u govorilka --no-pager -n "${2:-30}" $TAIL 2>&1 \
      | grep "govorilka\[" \
      | grep -E "heard:|reply:" \
      | sed -E 's/^([A-Za-z]+ [0-9]+ [0-9:]+) gt7-1 govorilka\[[0-9]+\]: /\1 /'
    exit 0
    ;;
  -w)    # wake word: только sleep/wake переходы и детект
    WAKE_ONLY=1
    ;;
esac

# Показываем события сна/бодрствования + ключевые события пайплайна.
FILTER="heard:|reply:|sending|out write|STT|brain|error|panic|handleUtterance|pkt decoded"

# Wake-word события: детект слова и переходы режимов (сон/бодрствование).
WAKE_FILTER="wake word|wake-word|sleeping|listening|idle timeout|молчи -> sleeping|noise for 5s"

# Скрываем шумный вывод Vosk (дочерний процесс загружает модель при старте).
VOSK_FILTER="VoskAPI|ReadDataFiles|RemoveOrphan|ComputeDerived|Loading|Done"

if [ "$WAKE_ONLY" = "1" ]; then
  journalctl --user -u govorilka --no-pager -n "${2:-30}" $TAIL 2>&1 \
    | grep "govorilka\[" \
    | grep -vE "$VOSK_FILTER" \
    | grep -E "$WAKE_FILTER" \
    | sed -E 's/^([A-Za-z]+ [0-9]+ [0-9:]+) gt7-1 govorilka\[[0-9]+\]: /\1 /'
  exit 0
fi

journalctl --user -u govorilka --no-pager -n "$N" $TAIL 2>&1 \
  | grep "govorilka\[" \
  | grep -vE "$VOSK_FILTER" \
  | grep -E "$FILTER|$WAKE_FILTER" \
  | sed -E 's/^([A-Za-z]+ [0-9]+ [0-9:]+) gt7-1 govorilka\[[0-9]+\]: /\1 /' \
  | awk -v nopkt="$NO_PKT" '{
      # -p: не показывать pkt вообще
      if (nopkt == 1 && $0 ~ /pkt decoded/) next;
      # Без -p: показываем pkt только когда громкость заметна (rms > 50)
      if ($0 ~ /rms=[0-9]+/) {
        match($0, /rms=[0-9]+/);
        rms = substr($0, RSTART+4, RLENGTH-4) + 0;
        if (rms < 50) next;
      }
      print;
    }'
