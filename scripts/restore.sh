#!/usr/bin/env bash
set -euo pipefail
umask 077
[[ $(id -u) == 0 && $# == 1 ]] || { echo 'Usage: sudo scripts/restore.sh /path/to/trusted-snapshot.tar.gz' >&2; exit 1; }
archive=$(realpath -- "$1")
set -a
. /etc/social-mcp/env
set +a
db=${SQLITE_PATH:-/var/lib/social-mcp/social.db}
media=${MEDIA_DIRECTORY:-/var/lib/social-mcp/media}
state=$(dirname -- "$db")
if [[ $media != "$state/media" || ! -f $db ]]; then
  echo 'For this script create an initial database first and keep MEDIA_DIRECTORY beside it as media/.' >&2
  exit 1
fi
exec 9>"$db.maintenance"
flock 9
stage=$(mktemp -d "$state/.restore.XXXXXXXX")
hold=$(mktemp -d "$state/pre-restore.XXXXXXXX")
trap 'rm -rf -- "$stage"' EXIT
tar -xzf "$archive" --no-same-owner --no-same-permissions -C "$stage"
[[ -f $stage/social.db && -d $stage/media ]]
SQLITE_PATH="$stage/social.db" MEDIA_DIRECTORY="$stage/media" /usr/local/bin/social-mcp verify
SQLITE_PATH="$stage/social.db" MEDIA_DIRECTORY="$stage/media" /usr/local/bin/social-mcp quarantine-restored
systemctl stop social-mcp.service
for entry in "$db" "$db-wal" "$db-shm" "$media"; do
  if [[ -e $entry ]]; then mv -- "$entry" "$hold/"; fi
done
chown -R social-mcp:social-mcp "$stage/social.db" "$stage/media"
mv -- "$stage/social.db" "$db"
mv -- "$stage/media" "$media"
printf 'Restored data and held previous files in %s\nReview uncertain deliveries before restarting with systemctl start social-mcp.\n' "$hold"
