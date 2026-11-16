# Fresh Ubuntu VPS

Use an Ubuntu server with a public hostname, SSH and TCP ports 80/443 open. Start with 2 vCPU, 4 GiB RAM and disk sized for uploads, WAL and backups. Install [Caddy natively](https://caddyserver.com/docs/install) and, if using WhatsApp, [Docker Engine](https://docs.docker.com/engine/install/ubuntu/) 28 or newer with the iptables firewall backend. Install local tools with `sudo apt install curl openssl iptables util-linux`. Caddy and Go run on the host.

1. Build with `make release` on a compatible build machine and check the checksum. Copy the project and binary to the VPS.
2. Run `sudo scripts/install.sh bin/social-mcp`. Set `PUBLIC_BASE_URL` and replace the example principals and account bindings in `/etc/social-mcp/env`. Three separate secrets are needed for the credential key, operator and agent. Generate each independently with `openssl rand -base64 32`. Keep the environment shell-compatible and accessible only to root. See [authentication](authentication.md).
3. For WhatsApp, edit `/etc/social-mcp/waha.env`. Use separate URL-safe values from `openssl rand -hex 32` for the API key and dashboard password. Put the same WAHA API key in the Go environment. Run `sudo scripts/install-waha.sh`, then link your account using the instructions in [integrations](integrations.md). Add only trusted domains to `WAHA_PREVIEW_HOSTS` after the script has installed bridge protections. Set the exact recipient IDs in `SOCIAL_CLIENTS`.
4. Replace `social.example.com` in `deployments/Caddyfile` with your public DNS hostname and install as `/etc/caddy/Caddyfile`. If Caddy already serves other sites, add this site's block to the existing config. Run `sudo caddy validate --config /etc/caddy/Caddyfile`, then `sudo systemctl enable --now caddy` and reload it.
5. Run `sudo systemctl enable --now social-mcp`. Check `curl -fsS http://127.0.0.1:8080/readyz` and `sudo systemctl status social-mcp`. Check that an unauthenticated request to the public `/mcp` returns 401, the authenticated MCP client can list tools and an HTTP request to public `/readyz` returns 404. The Go service binds only to loopback. Keep Caddy's normal upstream Host handling.
6. Connect the social accounts. Enable the read-only agent first and run its connection checks. Give it `publish: true` only for the account and chat IDs it should manage. Test publishing and the WhatsApp preview on destinations you control.
7. Enable daily snapshots with `sudo systemctl enable --now social-mcp-backup.timer`. The timer runs at 03:15 UTC. Use `sudo /usr/local/sbin/social-mcp-backup` for a manual snapshot and check `journalctl -u social-mcp-backup`.

Use `sudo systemctl restart social-mcp` and `sudo journalctl -u social-mcp -f` for routine operations. Test reboot recovery by scheduling a test post several minutes in the future, rebooting before it is due, then checking the campaign and platform. Verify that Docker exposes WAHA only at `127.0.0.1:3000`, and review the bridge rules with `sudo iptables -S DOCKER-USER` and `sudo iptables -S SOCIAL_WAHA_EGRESS`. The Docker firewall service must be active before the native Go service when WAHA is installed.

## Snapshots and restore

The backup script runs SQLite `VACUUM INTO`, copies exactly the snapshot's recorded media, verifies database integrity and file checksums, then publishes a compressed archive under `/var/backups/social-mcp`. Old archives are removed after about 14 days. Back up the environment and credential key separately and securely. WAHA's session directory is separate. For a WAHA session backup, stop its container and snapshot `/var/lib/waha/sessions` privately, or relink after recovery.

Restore with `sudo scripts/restore.sh /absolute/path/to/trusted-snapshot.tar.gz` from a deployment with existing state directories. The script verifies the extracted snapshot before stopping the service, holds the old database and media together, and leaves the service stopped after the swap. Previously unfinished deliveries are marked uncertain because the snapshot may predate successful sends. Compare with the providers before replacing those jobs. Start the service and inspect the status. Keep the prior state until recovery is verified.

The verified snapshot contains encrypted OAuth credentials. If an OAuth provider rotated a token after the snapshot, reconnect that account. Use `sudo bash -c 'set -a; . /etc/social-mcp/env; set +a; /usr/local/bin/social-mcp cleanup-media'` to remove expired unreferenced uploads. Media referenced by delivery history stays in place.

## Update and rollback

1. Build and verify the new binary. Run `sudo scripts/update.sh bin/social-mcp`. It snapshots SQLite and media, installs the binary, restarts systemd and checks readiness.
2. Read logs and verify existing deliveries. If the update fails and the schema is compatible, stop the service, atomically replace the binary with `/usr/local/bin/social-mcp.previous` and start it again.
3. If the updated database is incompatible with the previous binary, keep the failed version's state for reconciliation. Restore a compatible verified snapshot with the previous binary installed and use the restore procedure above.
4. For WAHA, record the current tag or digest, snapshot the stopped session directory if needed, update the pinned image in `waha.env` and run `sudo scripts/update-waha.sh`. The installer does a local authenticated health probe. `sudo docker stop social-waha` and `sudo scripts/start-waha.sh` are the ordinary stop/start operations.

For storage or bind address overrides, update Caddy, readiness commands, backup scripts and systemd writable paths together. The supplied scripts use the documented defaults.
