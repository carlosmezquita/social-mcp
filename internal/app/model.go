package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata"
)

var (
	ErrInvalid     = errors.New("invalid input")
	ErrForbidden   = errors.New("destination forbidden")
	ErrMissing     = errors.New("record not found")
	ErrConflict    = errors.New("operation conflicts with existing state")
	ErrUnavailable = errors.New("platform unavailable")
	identifier     = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)
	digitID        = regexp.MustCompile(`^[0-9]+$`)
	chatID         = regexp.MustCompile(`^[0-9]+(?:-[0-9]+)?@(?:c\.us|lid|g\.us)$`)
)

func invalid(message string) error { return fmt.Errorf("%w: %s", ErrInvalid, message) }
func id() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

type Target struct {
	Platform        string   `json:"platform"`
	Account         string   `json:"account_id"`
	Recipient       string   `json:"recipient_id,omitempty"`
	Text            string   `json:"text,omitempty"`
	URL             string   `json:"url,omitempty"`
	LinkTitle       string   `json:"link_title,omitempty"`
	LinkDescription string   `json:"link_description,omitempty"`
	MediaID         string   `json:"media_id,omitempty"`
	Preview         *bool    `json:"link_preview,omitempty"`
	Custom          *Preview `json:"custom_preview,omitempty"`
}
type Preview struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	ImageID     string `json:"image_id,omitempty"`
}
type Request struct {
	Key     string   `json:"idempotency_key"`
	Targets []Target `json:"targets"`
	At      string   `json:"scheduled_at,omitempty"`
	Zone    string   `json:"timezone,omitempty"`
}
type Edit struct {
	ID     string  `json:"id"`
	At     string  `json:"scheduled_at,omitempty"`
	Zone   string  `json:"timezone,omitempty"`
	Target *Target `json:"target,omitempty"`
}
type Filter struct {
	State    string    `json:"state,omitempty"`
	Platform string    `json:"platform,omitempty"`
	Account  string    `json:"account_id,omitempty"`
	From     time.Time `json:"from,omitempty"`
	To       time.Time `json:"to,omitempty"`
	Limit    int       `json:"limit,omitempty"`
	BeforeID string    `json:"before_id,omitempty"`
}
type Delivery struct {
	Binding     string    `json:"-"`
	ID          string    `json:"id"`
	Campaign    string    `json:"campaign_id"`
	Target      Target    `json:"target"`
	Scheduled   time.Time `json:"scheduled_at"`
	Local       string    `json:"scheduled_local"`
	Zone        string    `json:"timezone"`
	Due         time.Time `json:"next_attempt_at"`
	State       string    `json:"state"`
	Tries       int       `json:"attempts"`
	ExternalID  string    `json:"external_id,omitempty"`
	ExternalURL string    `json:"external_url,omitempty"`
	Failure     string    `json:"error,omitempty"`
}
type Attempt struct {
	Number  int        `json:"number"`
	Start   time.Time  `json:"start"`
	Finish  *time.Time `json:"finish,omitempty"`
	Outcome string     `json:"outcome,omitempty"`
	Detail  string     `json:"error,omitempty"`
}
type Media struct {
	ID   string `json:"id"`
	MIME string `json:"content_type"`
	Size int64  `json:"size"`
	SHA  string `json:"sha256"`
	Path string `json:"-"`
}
type Result struct{ ID, URL string }
type Failure struct {
	Kind, Message string
	After         time.Duration
}

func (f *Failure) Error() string         { return f.Message }
func fail(kind, message string) *Failure { return &Failure{Kind: kind, Message: message} }

type Adapter interface {
	Validate(Target, Media) error
	Publish(context.Context, Delivery, Account, Media, Media) (Result, error)
}

func schedule(value, zone string, now time.Time) (time.Time, string, error) {
	if zone == "" {
		zone = "UTC"
	}
	if zone == "Local" {
		return time.Time{}, zone, invalid("use an IANA timezone")
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, zone, invalid("unknown IANA timezone")
	}
	if value == "" {
		return now.UTC(), zone, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil || strings.HasSuffix(value, "-00:00") {
		return time.Time{}, zone, invalid("scheduled_at must be RFC3339 with an explicit known offset")
	}
	if zone != "UTC" {
		_, inputOffset := parsed.Zone()
		_, localOffset := parsed.In(loc).Zone()
		if inputOffset != 0 || !strings.HasSuffix(value, "Z") {
			if inputOffset != localOffset {
				return time.Time{}, zone, invalid("offset is invalid for the selected timezone at that time")
			}
		}
	}
	if !parsed.After(now) || parsed.Year() > 9998 {
		return time.Time{}, zone, invalid("scheduled_at must be in the future")
	}
	return parsed.UTC(), zone, nil
}
func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Opaque == "" && (u.Port() == "" || u.Port() == "443")
}
func bodyText(t Target) string {
	if t.URL == "" || strings.Contains(t.Text, t.URL) {
		return t.Text
	}
	return strings.TrimSpace(t.Text + " " + t.URL)
}
