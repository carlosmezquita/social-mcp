# Security controls

- Bind the backend and WAHA on loopback. Apply the installed WAHA firewall service before enabling automatic previews. The service keeps the SDK's loopback protection and validates external Host and Origin before routing.
- Use distinct random secrets and root-only environment files. OAuth tokens are protected in SQLite with AES-256-GCM using random nonces and account-associated data.
- Give automated principals only the exact scopes they need. Writes require a configured publishing grant. Account and chat scope and account identity are rechecked before dispatch.
- Bound inbound body sizes and rate-limit both the transport peer and individual principals. Provider redirects are disabled for Go HTTP clients. The Caddy config exposes only declared API, OAuth and signed-media paths.
- The Go service does not crawl arbitrary preview or media URLs. WAHA's preview fetch runs on a dedicated bridge with rules blocking private destinations and IPv6 reachable over that bridge. LinkedIn upload URLs come from the provider and are limited to LinkedIn upload domains over HTTPS.
- Keep access logs for sensitive OAuth and signed-media paths disabled unless you redact queries and headers. Application errors and logs omit raw provider response bodies and credentials.
- Backups contain post content and encrypted credentials. Protect snapshots and the independently stored credential encryption key as production data.
