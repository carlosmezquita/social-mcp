#!/usr/bin/env bash
set -euo pipefail
[[ $(id -u) == 0 && $# == 1 ]] || { echo 'Usage: sudo scripts/update.sh /path/to/new-social-mcp' >&2; exit 1; }
snapshot=$(/usr/local/sbin/social-mcp-backup)
echo "Saved $snapshot"
old=/usr/local/bin/social-mcp
install -m 0755 "$1" "$old.next"
cp -p -- "$old" "$old.previous"
mv -f -- "$old.next" "$old"
systemctl restart social-mcp.service
for ((try=0;try<15;try++)); do
  if curl -fsS --max-time 2 http://127.0.0.1:8080/readyz >/dev/null 2>&1; then exit 0; fi
  sleep 1
done
echo "Readiness failed. Previous binary is $old.previous. Inspect logs and use the rollback procedure in docs/deployment.md." >&2
exit 1
