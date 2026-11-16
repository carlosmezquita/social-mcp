package app

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type toolEnvelope[T any] struct {
	Items T `json:"items"`
}
type empty struct{}
type postID struct {
	ID string `json:"id"`
}
type chatRequest struct {
	Account string `json:"account_id"`
	Offset  int    `json:"offset,omitempty"`
}

func sortedAccounts(accounts map[string]Account) []string {
	keys := make([]string, 0, len(accounts))
	for k := range accounts {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
func addTool[I, O any](s *mcp.Server, name, description string, readOnly bool, f func(context.Context, Principal, I) (O, error)) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly}}, func(ctx context.Context, _ *mcp.CallToolRequest, input I) (*mcp.CallToolResult, toolEnvelope[O], error) {
		out, err := f(ctx, actor(ctx), input)
		if err != nil {
			_, message := publicError(err)
			return nil, toolEnvelope[O]{}, errors.New(message)
		}
		return nil, toolEnvelope[O]{Items: out}, nil
	})
}
func (a *Service) capabilities(ctx context.Context, p Principal) []fields {
	rows := a.httpAccounts(ctx, p, false)
	for _, row := range rows {
		ac := a.C.Accounts[row["id"].(string)]
		kinds := []string{"text", "link", "image"}
		if ac.Platform == "instagram" {
			kinds = []string{"image", "reel"}
		} else if ac.Video {
			kinds = append(kinds, "video")
		}
		row["content_types"] = kinds
		row["publishing_scope"] = p.Publish
		if ac.Platform == "whatsapp" {
			row["recipient_ids"] = p.Accounts[ac.ID]
			row["preview_hosts"] = a.C.PreviewHosts
		}
	}
	return rows
}

func (a *Service) mcp() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "social-mcp", Version: "0.1.0"}, nil)
	addTool(server, "list_accounts", "List accounts in your scope and local authorization status.", true, func(ctx context.Context, p Principal, _ empty) ([]fields, error) {
		return a.httpAccounts(ctx, p, false), nil
	})
	addTool(server, "publish_now", "Queue an immediate campaign. Use the same idempotency key when retrying a request.", false, func(ctx context.Context, p Principal, r Request) ([]Delivery, error) {
		return a.submit(ctx, p, r, false)
	})
	addTool(server, "schedule_post", "Schedule a campaign using an RFC3339 timestamp with explicit offset and optional IANA timezone.", false, func(ctx context.Context, p Principal, r Request) ([]Delivery, error) {
		return a.submit(ctx, p, r, true)
	})
	addTool(server, "list_scheduled_posts", "List upcoming deliveries with optional account, platform and date filters.", true, func(ctx context.Context, p Principal, f Filter) ([]Delivery, error) {
		f.State = "pending"
		return a.S.list(ctx, p, f)
	})
	addTool(server, "list_delivery_history", "List deliveries in scope, optionally filtered by state, account or platform.", true, func(ctx context.Context, p Principal, f Filter) ([]Delivery, error) { return a.S.list(ctx, p, f) })
	addTool(server, "get_post", "Inspect a delivery or campaign with its publishing attempts.", true, func(ctx context.Context, p Principal, i postID) ([]fields, error) {
		return a.detail(ctx, p, i.ID, true)
	})
	addTool(server, "get_publish_status", "Read delivery states and external publication IDs by campaign or delivery ID.", true, func(ctx context.Context, p Principal, i postID) ([]fields, error) {
		return a.detail(ctx, p, i.ID, false)
	})
	addTool(server, "update_scheduled_post", "Update pending content or schedule by delivery ID. A supplied target replaces the full payload.", false, func(ctx context.Context, p Principal, e Edit) ([]Delivery, error) { return a.change(ctx, p, e, false) })
	addTool(server, "cancel_scheduled_post", "Cancel a pending delivery by delivery ID.", false, func(ctx context.Context, p Principal, i postID) ([]Delivery, error) {
		return a.change(ctx, p, Edit{ID: i.ID}, true)
	})
	addTool(server, "get_platform_capabilities", "Inspect supported content types and your account and WhatsApp recipient scopes.", true, func(ctx context.Context, p Principal, _ empty) ([]fields, error) { return a.capabilities(ctx, p), nil })
	addTool(server, "list_whatsapp_chats", "List allowlisted WhatsApp chat/group IDs visible in the current chat page.", true, func(ctx context.Context, p Principal, i chatRequest) ([]Chat, error) {
		return a.chats(ctx, p, i.Account, i.Offset)
	})
	addTool(server, "check_connections", "Probe provider credentials and WhatsApp session connectivity.", true, func(ctx context.Context, p Principal, _ empty) ([]fields, error) {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return a.httpAccounts(ctx, p, true), nil
	})
	addTool(server, "preview_post", "Validate a scoped target and describe the requested provider submission without sending.", true, func(ctx context.Context, p Principal, t Target) (fields, error) {
		m, _, err := a.validate(ctx, p, t)
		if err != nil {
			return nil, err
		}
		mode := "none"
		if t.Platform == "whatsapp" && m.ID == "" {
			if t.Custom != nil {
				mode = "custom"
			} else if urlInText.MatchString(bodyText(t)) && (t.Preview == nil || *t.Preview) {
				mode = "automatic"
			}
		}
		return fields{"target": t, "text": bodyText(t), "media_type": m.MIME, "preview_mode": mode}, nil
	})
	addTool(server, "get_whatsapp_ack", "Read WhatsApp server/delivery/read acknowledgement for a known message ID.", true, func(ctx context.Context, p Principal, i postID) (fields, error) {
		ds, err := a.S.get(ctx, p, i.ID)
		if err != nil {
			return nil, err
		}
		if len(ds) != 1 || ds[0].ID != i.ID || ds[0].ExternalID == "" || ds[0].Target.Platform != "whatsapp" {
			return nil, ErrMissing
		}
		return a.whatsAppAck(ctx, ds[0])
	})
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
}

func (a *Service) whatsAppAck(ctx context.Context, d Delivery) (fields, error) {
	account := a.C.Accounts[d.Target.Account]
	if account.Platform != "whatsapp" || strings.TrimSpace(d.ExternalID) == "" {
		return nil, ErrUnavailable
	}
	return a.Adapters["whatsapp"].(*platform).ack(ctx, account, d)
}
