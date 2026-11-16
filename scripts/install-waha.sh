#!/usr/bin/env bash
set -euo pipefail
project=$(cd -- "$(dirname -- "$0")/.." && pwd)
config=/etc/social-mcp/waha.env
[[ $(id -u) == 0 ]] || { echo 'Run with sudo.' >&2; exit 1; }
[[ $(stat -c '%u:%a' "$config") == 0:600 ]] || { echo 'WAHA environment file must be root-owned and mode 0600.' >&2; exit 1; }
read_setting() { sed -n "s/^$1=//p" "$config" | tail -n 1; }
image=$(read_setting WAHA_IMAGE)
key=$(read_setting WAHA_API_KEY)
password=$(read_setting WAHA_DASHBOARD_PASSWORD)
[[ $image =~ ^devlikeapro/waha(-plus)?(:(latest-|arm-)?[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?|@sha256:[a-f0-9]{64})$ ]] || { echo 'Set WAHA_IMAGE to a versioned WAHA image or digest.' >&2; exit 1; }
[[ $key =~ ^[a-zA-Z0-9_-]{32,}$ && $password =~ ^[a-zA-Z0-9_-]{20,}$ && $(read_setting WHATSAPP_DEFAULT_ENGINE) == GOWS ]] || { echo 'Set a strong API key, dashboard password and GOWS engine.' >&2; exit 1; }
docker_major=$(docker version --format '{{.Server.Version}}' | cut -d. -f1)
[[ $docker_major =~ ^[0-9]+$ && $docker_major -ge 28 ]] || { echo 'Docker Engine 28 or newer is required for localhost-only published ports.' >&2; exit 1; }
docker pull "$image"
install -d -m 0700 /var/lib/waha/sessions /var/lib/waha/media
if ! docker network inspect social-waha >/dev/null 2>&1; then
  docker network create --driver bridge --ipv6=false --opt com.docker.network.bridge.name=br-social-waha social-waha >/dev/null
fi
[[ $(docker network inspect -f '{{.Driver}}:{{index .Options "com.docker.network.bridge.name"}}:{{.EnableIPv6}}' social-waha) == bridge:br-social-waha:false ]] || { echo 'Unexpected Docker bridge configuration.' >&2; exit 1; }
install -m 0755 "$project/scripts/waha-firewall.sh" /usr/local/sbin/social-mcp-waha-firewall
install -m 0644 "$project/deployments/social-mcp-waha-firewall.service" /etc/systemd/system/
install -d -m 0755 /etc/systemd/system/social-mcp.service.d
install -m 0644 "$project/deployments/social-mcp-waha.conf" /etc/systemd/system/social-mcp.service.d/waha.conf
systemctl daemon-reload
systemctl enable social-mcp-waha-firewall.service
systemctl restart social-mcp-waha-firewall.service
if docker container inspect social-waha-previous >/dev/null 2>&1; then
  echo 'Review the existing social-waha-previous container before updating.' >&2
  exit 1
fi
previous=false
if docker container inspect social-waha >/dev/null 2>&1; then
  docker stop social-waha >/dev/null
  docker rename social-waha social-waha-previous
  previous=true
fi
rollback() {
  docker rm -f social-waha 2>/dev/null || true
  if "$previous"; then
    docker rename social-waha-previous social-waha
    docker start social-waha
  fi
}
if ! docker run --detach --name social-waha --restart unless-stopped \
    --network social-waha --publish 127.0.0.1:3000:3000 --env-file "$config" \
    --security-opt no-new-privileges:true \
    --mount type=bind,source=/var/lib/waha/sessions,target=/app/.sessions \
    --mount type=bind,source=/var/lib/waha/media,target=/app/.media "$image" >/dev/null; then
  rollback
  exit 1
fi
for ((attempt=0;attempt<20;attempt++)); do
  if printf 'header = "X-Api-Key: %s"\n' "$key" | curl -fsS --max-time 3 --config - http://127.0.0.1:3000/api/sessions >/dev/null 2>&1; then
    if "$previous"; then docker rm social-waha-previous >/dev/null; fi
    echo 'WAHA API is healthy. Check session status and link the phone if needed.'
    exit 0
  fi
  sleep 2
done
echo 'WAHA did not become healthy. Rolling back the container.' >&2
rollback
exit 1
