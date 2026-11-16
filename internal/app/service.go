package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const publishTimeout = 6 * time.Minute
const leaseDuration = publishTimeout + 30*time.Second

type Service struct {
	C          Config
	S          *Store
	Log        *slog.Logger
	Adapters   map[string]Adapter
	Vault      *Vault
	wake       chan struct{}
	authClient *http.Client
}

func New(c Config, s *Store, log *slog.Logger) *Service {
	a := &Service{C: c, S: s, Log: log, wake: make(chan struct{}, 1), authClient: providerClient()}
	a.Vault = newVault(s, c.Key)
	a.Adapters = map[string]Adapter{}
	for _, name := range []string{"whatsapp", "linkedin", "instagram", "x"} {
		a.Adapters[name] = &platform{kind: name, app: a, http: providerClient(), base: providerBase(name, c)}
	}
	return a
}
func (a *Service) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}
func (a *Service) validate(ctx context.Context, p Principal, t Target) (Media, Media, error) {
	if !p.allows(t) {
		return Media{}, Media{}, ErrForbidden
	}
	account, ok := a.C.Accounts[t.Account]
	adapter := a.Adapters[t.Platform]
	if !ok || account.Platform != t.Platform || adapter == nil {
		return Media{}, Media{}, invalid("account does not match platform")
	}
	if (t.Platform == "whatsapp" && !chatID.MatchString(t.Recipient)) || (t.Platform != "whatsapp" && (t.Recipient != "" || t.Preview != nil || t.Custom != nil)) {
		return Media{}, Media{}, invalid("recipient and preview fields apply to WhatsApp only")
	}
	if len(t.Text) > 40000 || len(t.URL) > 2048 || (t.URL != "" && !validURL(t.URL)) {
		return Media{}, Media{}, invalid("invalid text length or HTTPS URL")
	}
	if t.LinkTitle != "" || t.LinkDescription != "" {
		if t.Platform != "linkedin" || t.URL == "" || t.MediaID != "" || strings.TrimSpace(t.LinkTitle) == "" || len(t.LinkTitle) > 200 || len(t.LinkDescription) > 500 {
			return Media{}, Media{}, invalid("LinkedIn article metadata requires a URL and title, with no separate post media")
		}
	}
	var media, preview Media
	var err error
	if t.MediaID != "" {
		media, err = a.S.media(ctx, p.ID, t.MediaID, p.Admin)
		if err != nil {
			return media, preview, err
		}
	}
	if t.Custom != nil {
		if t.MediaID != "" || (t.Preview != nil && !*t.Preview) || t.URL == "" || strings.TrimSpace(t.Custom.Title) == "" || len(t.Custom.Title) > 300 || len(t.Custom.Description) > 1000 {
			return media, preview, invalid("custom preview requires a URL and title and cannot be combined with post media or disabled previews")
		}
		if t.Custom.ImageID != "" {
			preview, err = a.S.media(ctx, p.ID, t.Custom.ImageID, p.Admin)
			if err != nil {
				return media, preview, err
			}
			if !strings.HasPrefix(preview.MIME, "image/") || preview.Size > 1024*1024 {
				return media, preview, invalid("preview image must be 1 MiB or smaller")
			}
		}
	}
	if media.MIME == "video/mp4" && t.Platform != "instagram" && !account.Video {
		return media, preview, invalid("video publishing is disabled for this account")
	}
	return media, preview, adapter.Validate(t, media)
}
func (a *Service) submit(ctx context.Context, p Principal, r Request, scheduled bool) ([]Delivery, error) {
	if !p.Publish {
		return nil, ErrForbidden
	}
	if !identifier.MatchString(r.Key) || len(r.Targets) == 0 || len(r.Targets) > 20 {
		return nil, invalid("use a 1–80 character idempotency key and 1–20 targets")
	}
	if scheduled != (r.At != "") {
		return nil, invalid("schedule requests require scheduled_at; immediate requests omit it")
	}
	if key, err := a.S.existing(ctx, p, r); err != nil {
		return nil, err
	} else if key != "" {
		return a.S.get(ctx, p, key)
	}
	at, zone, err := schedule(r.At, r.Zone, time.Now())
	if err != nil {
		return nil, err
	}
	seen := map[[2]string]bool{}
	for _, t := range r.Targets {
		k := [2]string{t.Account, t.Recipient}
		if seen[k] {
			return nil, invalid("a campaign may target a destination only once")
		}
		seen[k] = true
		if _, _, err = a.validate(ctx, p, t); err != nil {
			return nil, err
		}
		status := a.connection(ctx, a.C.Accounts[t.Account], false)
		if status == "unconfigured" || status == "disconnected" || status == "authorization_required" {
			return nil, ErrUnavailable
		}
	}
	campaign, err := a.S.create(ctx, p, r, at, zone, a.C.Accounts)
	if err != nil {
		return nil, err
	}
	a.signal()
	a.Log.Info("campaign stored", "campaign", campaign, "actor", p.ID)
	return a.S.get(ctx, p, campaign)
}
func (a *Service) change(ctx context.Context, p Principal, e Edit, cancel bool) ([]Delivery, error) {
	if !p.Publish {
		return nil, ErrForbidden
	}
	ds, err := a.S.get(ctx, p, e.ID)
	if err != nil {
		return nil, err
	}
	if len(ds) != 1 || ds[0].ID != e.ID {
		return nil, invalid("use a delivery ID")
	}
	d := ds[0]
	if cancel {
		if err = a.S.mutate(ctx, p, d, d.Target, d.Scheduled, d.Zone, true); err != nil {
			return nil, err
		}
	} else {
		t := d.Target
		if e.Target != nil {
			t = *e.Target
		}
		if t.Account != d.Target.Account || t.Platform != d.Target.Platform || t.Recipient != d.Target.Recipient {
			return nil, invalid("destination cannot be changed")
		}
		if _, _, err = a.validate(ctx, p, t); err != nil {
			return nil, err
		}
		at, zone := d.Scheduled, d.Zone
		if e.At != "" {
			if e.Zone == "" {
				e.Zone = d.Zone
			}
			at, zone, err = schedule(e.At, e.Zone, time.Now())
			if err != nil {
				return nil, err
			}
		} else if e.Target == nil || e.Zone != "" {
			return nil, invalid("provide content or a replacement scheduled_at")
		}
		if d.State == "retry_wait" && at.Before(d.Due) {
			at = d.Due
		}
		if err = a.S.mutate(ctx, p, d, t, at, zone, false); err != nil {
			return nil, err
		}
		a.signal()
	}
	return a.S.get(ctx, p, e.ID)
}
func (a *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(a.C.Poll)
	defer ticker.Stop()
	a.Log.Info("scheduler started")
	for {
		if ctx.Err() != nil {
			return
		}
		d, err := a.S.claim(ctx, time.Now(), leaseDuration)
		if err == nil {
			a.execute(ctx, d)
			continue
		}
		if !errors.Is(err, ErrMissing) && ctx.Err() == nil {
			a.Log.Error("job claim failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-a.wake:
		}
	}
}
func (a *Service) execute(parent context.Context, d Delivery) {
	a.Log.Info("publication attempt started", "delivery", d.ID, "attempt", d.Tries)
	ctx, cancel := context.WithTimeout(parent, publishTimeout)
	defer cancel()
	var actor string
	err := a.S.DB.QueryRowContext(ctx, "SELECT actor FROM campaigns WHERE id=?", d.Campaign).Scan(&actor)
	var p Principal
	for _, candidate := range a.C.Principals {
		if candidate.ID == actor {
			p = candidate
			break
		}
	}
	var result Result
	if err != nil {
		err = fail("transient", "could not read campaign owner")
	} else if !p.Publish || !p.allows(d.Target) || d.Binding != a.C.Accounts[d.Target.Account].binding() {
		err = fail("permanent", "publishing scope revoked or provider identity changed")
	} else {
		var media, preview Media
		media, preview, err = a.validate(ctx, p, d.Target)
		if err != nil {
			err = fail("permanent", "stored delivery no longer validates")
		} else {
			result, err = a.Adapters[d.Target.Platform].Publish(ctx, d, a.C.Accounts[d.Target.Account], media, preview)
		}
	}
	state, kind, message, due := "published", "published", "", d.Due
	if err != nil {
		f := fail("ambiguous", "publishing outcome unknown")
		var classified *Failure
		if errors.As(err, &classified) {
			f = classified
		}
		kind, message = f.Kind, f.Message
		switch f.Kind {
		case "transient", "rate_limited":
			state = "failed"
			if d.Tries <= a.C.Retries {
				state = "retry_wait"
				delay := min(20*time.Minute, 5*time.Second*time.Duration(1<<min(d.Tries-1, 10)))
				due = time.Now().Add(max(delay, f.After))
			}
		case "ambiguous":
			state = "uncertain"
		default:
			state = "failed"
		}
	}
	save, stop := context.WithTimeout(context.WithoutCancel(parent), 8*time.Second)
	defer stop()
	if err := a.S.finish(save, d, result, state, kind, message, due); err != nil {
		a.Log.Error("publication result could not be stored", "delivery", d.ID)
		return
	}
	a.Log.Info("publication attempt finished", "delivery", d.ID, "state", state, "class", kind)
}
