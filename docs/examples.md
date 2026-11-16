# Publishing examples

Connect the agent to `https://social.example.com/mcp`. Examples use the aliases `linkedin`, `x`, `instagram` and `wa`. Set unique, stable idempotency keys per intended campaign and reuse them on retries. Replace sample recipients with IDs in your scope. When copying a scheduled example, choose a future RFC3339 time including its explicit offset. For daylight saving transitions, give the intended offset or an unambiguous UTC timestamp with `Z`.

To upload media, send JPEG, PNG or MP4 bytes using the same principal that will publish.

```bash
curl -fsS https://social.example.com/api/media \
  -H "Authorization: Bearer $SOCIAL_AGENT_TOKEN" \
  -H 'Content-Type: image/jpeg' --data-binary @photo.jpg
```

Use the returned `id` as the delivery's `media_id`. To add an image to a WhatsApp custom preview, use its ID inside `custom_preview.image_id` instead.

## Publish on LinkedIn now

Call `publish_now`.

```json
{
  "idempotency_key": "demo-linkedin-announcement-v1",
  "targets": [
    {"platform": "linkedin", "account_id": "linkedin", "text": "Registration for our build night is open.", "url": "https://example.com/build-night"}
  ]
}
```

## Publish on X now

Call `publish_now`.

```json
{
  "idempotency_key": "demo-x-announcement-v1",
  "targets": [
    {"platform": "x", "account_id": "x", "text": "Registration is open.", "url": "https://example.com/build-night"}
  ]
}
```

## Schedule an Instagram image

Call `schedule_post`.

```json
{
  "idempotency_key": "demo-instagram-photo-v1",
  "scheduled_at": "2027-02-18T18:30:00+00:00",
  "timezone": "Europe/London",
  "targets": [
    {"platform": "instagram", "account_id": "instagram", "text": "Build night starts at 7 PM.", "media_id": "REPLACE_WITH_UPLOAD_ID"}
  ]
}
```

## Schedule a WhatsApp message with a preview

Add `example.com` to `WAHA_PREVIEW_HOSTS` for this example or use your own approved domain, then call `schedule_post`.

```json
{
  "idempotency_key": "demo-whatsapp-reminder-v1",
  "scheduled_at": "2027-02-18T18:00:00+00:00",
  "timezone": "Europe/London",
  "targets": [
    {"platform": "whatsapp", "account_id": "wa", "recipient_id": "44123456789@c.us", "text": "Here are the details for tonight.", "url": "https://example.com/build-night"}
  ]
}
```

Previews are requested by default for supported text-only WhatsApp links. To send known metadata once through the custom endpoint, supply the title with the link.

```json
{
  "platform": "whatsapp",
  "account_id": "wa",
  "recipient_id": "44123456789@c.us",
  "text": "Here are the details for tonight.",
  "url": "https://example.com/build-night",
  "custom_preview": {"title": "February build night", "description": "Join us at 7 PM."}
}
```

## Send a WhatsApp group message

Call `publish_now`.

```json
{
  "idempotency_key": "demo-group-message-v1",
  "targets": [
    {"platform": "whatsapp", "account_id": "wa", "recipient_id": "1234-5678@g.us", "text": "Doors open at 6:45."}
  ]
}
```

## Schedule a campaign across platforms

Call `schedule_post`.

```json
{
  "idempotency_key": "demo-multiplatform-build-night-v1",
  "scheduled_at": "2027-02-18T17:00:00+00:00",
  "timezone": "Europe/London",
  "targets": [
    {"platform": "linkedin", "account_id": "linkedin", "text": "Join our build night this evening."},
    {"platform": "x", "account_id": "x", "text": "Build night tonight at 7."},
    {"platform": "whatsapp", "account_id": "wa", "recipient_id": "1234-5678@g.us", "text": "See you at 7."}
  ]
}
```

## Manage deliveries

| Action | MCP call | REST equivalent |
| --- | --- | --- |
| Upcoming posts | `list_scheduled_posts {"limit":25}` | `GET /api/deliveries?state=pending&limit=25` |
| Delivery history | `list_delivery_history {"state":"failed"}` | `GET /api/deliveries?state=failed` |
| Inspect an individual failure | `get_post {"id":"DELIVERY_ID"}` | `GET /api/deliveries/DELIVERY_ID` |
| Campaign status | `get_publish_status {"id":"CAMPAIGN_ID"}` | `GET /api/campaigns/CAMPAIGN_ID` |
| Change a time | `update_scheduled_post {"id":"DELIVERY_ID","scheduled_at":"2027-02-18T19:00:00Z"}` | `PATCH /api/deliveries/DELIVERY_ID` with `scheduled_at` |
| Cancel a pending delivery | `cancel_scheduled_post {"id":"DELIVERY_ID"}` | `DELETE /api/deliveries/DELIVERY_ID` |
| Check WhatsApp acknowledgement | `get_whatsapp_ack {"id":"DELIVERY_ID"}` | `GET /api/deliveries/DELIVERY_ID/ack` |

For REST publishing, the same JSON payload goes to `POST /api/publish` or `POST /api/schedule` with `Content-Type: application/json` and the bearer header. Those routes return HTTP 202 and delivery records. To edit content, supply a complete replacement `target` while preserving the account, platform and recipient. Use a delivery ID for edits and cancellations. List `retry_wait`, `failed` and `uncertain` separately when investigating problems.
