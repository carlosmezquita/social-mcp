# Authentication and permissions

The file `/etc/social-mcp/env` holds root-protected configuration. It uses shell-compatible `NAME=value` syntax. The systemd service loads it without exposing it to clients. Encrypting provider tokens depends on retaining the configured 32-byte AES-GCM key, which lives outside SQLite.

## Agent scope

Use an operator credential when connecting providers. Give it `admin: true` and only the accounts it manages. An operator can keep `publish: false`. A separate marketing principal starts with `publish: false`. Add the required account aliases to its `accounts` map and explicitly enable publishing. For each WhatsApp alias list the exact chat IDs, such as `44123456789@c.us` and `1234-5678@g.us`. An empty WhatsApp list authorizes no recipients. A LinkedIn, X or Instagram alias uses an empty array to authorize that account.

Principal IDs should be stable. Changing a token retains its principal's scope and history. Removing an account, recipient or principal scope prevents pending jobs from dispatching on their next attempt. Keep aliases bound to the same provider identity. The worker compares the queued account binding to the current configuration.

## Connect Grok Bot or another MCP client

Point the remote client at `https://social.example.com/mcp`. For a client that supports authenticated header configuration, use `Authorization: Bearer <agent-token>`. Test `list_accounts`, then `preview_post` with an authorized test target. The [Grok connector documentation](https://docs.x.ai/grok/connectors) describes registering a public MCP server. The exact authentication options available in your Grok Bot workspace should be checked when adding the connector.

For a client that requires OAuth discovery, use an existing HTTPS authorization server and configure `MCP_OAUTH_ISSUER`, `MCP_OAUTH_INTROSPECTION_URL`, `MCP_OAUTH_CLIENT_ID` and `MCP_OAUTH_CLIENT_SECRET`. Register `https://social.example.com/mcp` as the resource and grant the `social:mcp` scope. The authorization server must support the client's registration or preregistration flow, authorization code with PKCE and OAuth protected-resource audiences. Introspection must return `active`, `iss`, `sub`, `exp`, `aud` and `scope`. Use `oauth_subject` in `SOCIAL_CLIENTS` to bind the returned subject to a local policy. At runtime the authorization server issues tokens and social-mcp verifies the token audience and local scope. OAuth is optional for clients that already support a private header.

If the client cannot use either mode, send requests through the authenticated REST API from the agent runtime. Do not publish the service without application authentication.

## Provider OAuth flow

Add the provider app credentials and target account binding first. Set these HTTPS redirect URIs in the corresponding provider console.

| Provider | Redirect URI |
| --- | --- |
| LinkedIn | `https://social.example.com/oauth/linkedin/callback` |
| X | `https://social.example.com/oauth/x/callback` |
| Instagram | `https://social.example.com/oauth/instagram/callback` |

Request a one-time browser URL with the scoped operator token.

```bash
curl -fsS -X POST \
  -H "Authorization: Bearer $SOCIAL_OPERATOR_TOKEN" \
  https://social.example.com/connect/linkedin
```

Open the returned URL in your browser within two minutes and authorize the specified provider account. Use the account alias in the path. The browser callback is tied to a single-use server-side state and a secure browser cookie. For X, the exchange also uses PKCE. The service verifies the provider identity or approved organization role before storing the token. If account setup fails, create a new one-time URL after correcting the provider app or configured identity.

No provider write is used to probe connectivity. `check_connections` checks token identity and session status. Actual write permissions, LinkedIn product access and X billing are enforced by the provider at publish time.
