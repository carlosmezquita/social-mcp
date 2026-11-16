# Troubleshooting

| Symptom | Check |
| --- | --- |
| Local readiness fails | Confirm service env, state directory ownership and SQLite path. Inspect `journalctl -u social-mcp`. |
| Public MCP returns 401 | Confirm the Bearer header or a token issued to the MCP resource audience. |
| Public MCP returns 403 or 421 | Preserve the external Host through Caddy and check the request Origin against `PUBLIC_BASE_URL`. |
| Tool reports destination forbidden | Inspect account aliases and WhatsApp IDs for the specific principal, and whether `publish` is enabled. |
| Provider unavailable | Check app credentials, local account bindings, expiry and the provider's developer console. Use `check_connections`. |
| WAHA session is disconnected | Check Dashboard through SSH, filter chats by the active session and relink if asked by WAHA. The worker retries disconnection using backoff. |
| Instagram media remains pending | Test the public signed URL through Caddy when it is generated. Check Meta media format, size and processing status. |
| Request remains in retry_wait | Review the latest attempt and its next timestamp. Rate-limit responses can delay later attempts. |
| Delivery is uncertain | Search the destination or provider interface for the expected post or chat message before creating another delivery. Confirm the check with an operator where necessary. |
| WhatsApp message arrived with no preview | Confirm the page exposes OG metadata, its host is configured and the thumbnail is reachable. If extraction fails use custom metadata for a future send. |
