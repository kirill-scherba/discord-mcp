#!/bin/bash
# govlog-ru — просмотр лога говорилки на RU VPS (ru.bmat.uk / reg.ru).
# Использование: как govlog.sh (-p, -qa, -w), но по SSH на RU сервер.
SOCK=~/.ssh/agent/$(ls ~/.ssh/agent/ | head -1)
SSH_AUTH_SOCK=$SOCK ssh root@194.58.95.91 "journalctl -u govorilka --no-pager -n 50 $1 $2 2>&1 | grep -vE 'pkt decoded|pkt[0-9]' | tail -30"
