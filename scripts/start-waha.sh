#!/usr/bin/env bash
set -euo pipefail
systemctl restart social-mcp-waha-firewall.service
exec docker start social-waha
