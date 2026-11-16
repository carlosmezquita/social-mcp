package app

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Chat struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Group bool   `json:"group"`
}

func (p *platform) working(ctx context.Context, a Account) error {
	if p.app.C.WAHAKey == "" {
		return fail("auth", "WAHA API key missing")
	}
	var status struct{ Status string }
	if _, err := p.json(ctx, http.MethodGet, "/api/sessions/"+url.PathEscape(a.Session), "", nil, &status, false); err != nil {
		return err
	}
	if status.Status != "WORKING" {
		return fail("transient", "WhatsApp session is disconnected")
	}
	return nil
}
func (a *Service) chats(ctx context.Context, principal Principal, account string, offset int) ([]Chat, error) {
	if offset < 0 || offset > 100000 {
		return nil, invalid("invalid chat offset")
	}
	ac, ok := a.C.Accounts[account]
	if _, allowed := principal.Accounts[account]; !ok || !allowed || ac.Platform != "whatsapp" {
		return nil, ErrForbidden
	}
	p := a.Adapters["whatsapp"].(*platform)
	if err := p.working(ctx, ac); err != nil {
		return nil, ErrUnavailable
	}
	var rows []Chat
	path := "/api/" + url.PathEscape(ac.Session) + "/chats/overview?limit=100&offset=" + strconv.Itoa(offset)
	if _, err := p.json(ctx, http.MethodGet, path, "", nil, &rows, false); err != nil {
		return nil, ErrUnavailable
	}
	out := []Chat{}
	for _, c := range rows {
		if !chatID.MatchString(c.ID) || !principal.allows(Target{Platform: "whatsapp", Account: account, Recipient: c.ID}) {
			continue
		}
		c.Group = strings.HasSuffix(c.ID, "@g.us")
		out = append(out, c)
	}
	return out, nil
}
func wahaFile(m Media) (fields, error) {
	b, err := os.ReadFile(m.Path)
	if err != nil {
		return nil, fail("permanent", "stored media cannot be read")
	}
	extension := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "video/mp4": ".mp4"}[m.MIME]
	return fields{"mimetype": m.MIME, "filename": m.ID + extension, "data": base64.StdEncoding.EncodeToString(b)}, nil
}
func (p *platform) whatsapp(ctx context.Context, d Delivery, a Account, m, preview Media) (Result, error) {
	if err := p.working(ctx, a); err != nil {
		return Result{}, err
	}
	t := d.Target
	text := bodyText(t)
	payload := fields{"session": a.Session, "chatId": t.Recipient, "text": text}
	path := "/api/sendText"
	switch {
	case m.ID != "":
		file, err := wahaFile(m)
		if err != nil {
			return Result{}, err
		}
		path = "/api/sendImage"
		if m.MIME == "video/mp4" {
			path = "/api/sendVideo"
		}
		delete(payload, "text")
		payload["file"], payload["caption"] = file, text
	case t.Custom != nil:
		path = "/api/send/link-custom-preview"
		payload["linkPreviewHighQuality"] = true
		metadata := fields{"url": t.URL, "title": t.Custom.Title, "description": t.Custom.Description}
		if preview.ID != "" {
			file, err := wahaFile(preview)
			if err != nil {
				return Result{}, err
			}
			metadata["image"] = file
		}
		payload["preview"] = metadata
		p.app.Log.Info("requested WhatsApp custom preview", "delivery", d.ID)
	default:
		enabled := urlInText.MatchString(text) && (t.Preview == nil || *t.Preview)
		payload["linkPreview"], payload["linkPreviewHighQuality"] = enabled, enabled
	}
	var reply struct {
		ID string `json:"id"`
	}
	_, err := p.json(ctx, http.MethodPost, path, "", payload, &reply, true)
	return confirmed(reply.ID, err)
}

func (p *platform) ack(ctx context.Context, account Account, d Delivery) (fields, error) {
	if err := p.working(ctx, account); err != nil {
		return nil, ErrUnavailable
	}
	var reply struct {
		ID      string
		Ack     *int
		AckName string
	}
	path := "/api/" + url.PathEscape(account.Session) + "/chats/" + url.PathEscape(d.Target.Recipient) + "/messages/" + url.PathEscape(d.ExternalID)
	if _, err := p.json(ctx, http.MethodGet, path, "", nil, &reply, false); err != nil {
		return nil, ErrUnavailable
	}
	return fields{"message_id": d.ExternalID, "ack": reply.Ack, "ack_name": reply.AckName}, nil
}
