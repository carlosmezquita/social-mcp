#!/usr/bin/env bash
set -euo pipefail
umask 077
[[ $(id -u) == 0 ]] || { echo 'Run with sudo.' >&2; exit 1; }
# This is a root-owned shell-compatible EnvironmentFile.
set -a
. /etc/social-mcp/env
set +a
db=${SQLITE_PATH:-/var/lib/social-mcp/social.db}
media=${MEDIA_DIRECTORY:-/var/lib/social-mcp/media}
backup=${BACKUP_DIRECTORY:-/var/backups/social-mcp}
install -d -m 0700 "$backup"
exec 9>"$db.maintenance"
flock 9
staging=$(mktemp -d "$backup/.staging.XXXXXXXX")
trap 'rm -rf -- "$staging"' EXIT
/usr/local/bin/social-mcp backup "$staging/social.db"
mkdir "$staging/media"
SQLITE_PATH="$staging/social.db" MEDIA_DIRECTORY="$staging/media" /usr/local/bin/social-mcp copy-snapshot-media "$media"
SQLITE_PATH="$staging/social.db" MEDIA_DIRECTORY="$staging/media" /usr/local/bin/social-mcp verify
archive="$backup/social-mcp-$(date -u +%Y%m%dT%H%M%S)-${staging##*.}.tar.gz"
tar -C "$staging" -czf "$archive.part" social.db media
mv -- "$archive.part" "$archive"
find "$backup" -maxdepth 1 -name 'social-mcp-*.tar.gz' -type f -mtime +13 -delete
printf '%s\n' "$archive"
