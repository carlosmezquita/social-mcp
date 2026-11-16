#!/usr/bin/env bash
set -euo pipefail
project=$(cd -- "$(dirname -- "$0")/.." && pwd)
[[ $(id -u) == 0 && $# == 1 ]] || { echo 'Usage: sudo scripts/install.sh /path/to/social-mcp' >&2; exit 1; }
getent passwd social-mcp >/dev/null || useradd --system --user-group --home-dir /var/lib/social-mcp --shell /usr/sbin/nologin social-mcp
install -d -m 0700 /etc/social-mcp /var/backups/social-mcp
install -d -o social-mcp -g social-mcp -m 0700 /var/lib/social-mcp /var/lib/social-mcp/media
[[ -e /etc/social-mcp/env ]] || install -m 0600 "$project/.env.example" /etc/social-mcp/env
[[ -e /etc/social-mcp/waha.env ]] || install -m 0600 "$project/deployments/waha.env.example" /etc/social-mcp/waha.env
[[ ! -e /usr/local/bin/social-mcp ]] || { echo 'Existing binary found. Use scripts/update.sh.' >&2; exit 1; }
install -m 0755 "$1" /usr/local/bin/social-mcp
install -m 0644 "$project/deployments/social-mcp.service" "$project/deployments/social-mcp-backup.service" "$project/deployments/social-mcp-backup.timer" /etc/systemd/system/
install -m 0755 "$project/scripts/backup.sh" /usr/local/sbin/social-mcp-backup
systemctl daemon-reload
echo 'Configure /etc/social-mcp/env, then run systemctl enable --now social-mcp.'
