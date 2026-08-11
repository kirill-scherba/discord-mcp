#!/bin/bash
# govlog — удобный просмотр лога говорилки: только ключевые события.
#
# Usage:
#   govlog            — последние события
#   govlog -f         — следить (tail -f)
#   govlog -n 50      — последние 50 событий
#
# Показывает: heard (что распознано), reply (ответ Барона),
# sending (сколько фреймов отправлено), ошибки, длительность фраз.
N="${2:-30}"
TAIL=""

case "$1" in
  -f|-F) TAIL="-f" ;;
  -qa)   # вопрос → ответ, только heard и reply
    journalctl --user -u govorilka --no-pager -n "${2:-30}" $TAIL 2>&1 \
      | grep "govorilka\[" \
      | grep -E "heard:|reply:" \
      | sed -E 's/^([A-Za-z]+ [0-9]+ [0-9:]+) gt7-1 govorilka\[[0-9]+\]: /\1 /'
    exit 0
    ;;
esac

journalctl --user -u govorilka --no-pager -n "$N" $TAIL 2>&1 \
  | grep "govorilka\[" \
  | grep -E "heard:|reply:|sending|out write|STT|brain|error|panic|handleUtterance|pkt decoded" \
  | sed -E 's/^([A-Za-z]+ [0-9]+ [0-9:]+) gt7-1 govorilka\[[0-9]+\]: /\1 /' \
  | awk '{
      # Показываем pkt только когда громкость заметна (rms > 50),
      # иначе лог забивается тишиной (rms=0).
      if ($0 ~ /rms=[0-9]+/) {
        match($0, /rms=[0-9]+/);
        rms = substr($0, RSTART+4, RLENGTH-4) + 0;
        if (rms < 50) next;
      }
      print;
    }'
