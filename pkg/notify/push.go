package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/notify/webpush"
)

// Web Push urgencies (RFC 8030 section 5.3).
const (
	UrgencyVeryLow = "very-low"
	UrgencyLow     = "low"
	UrgencyNormal  = "normal"
	UrgencyHigh    = "high"
)

// PushTTL is how long a push service keeps a message for an offline browser.
const PushTTL = 24 * 60 * 60

// ErrGone marks a subscription the push service no longer knows (404, 410): the device must go. It also
// matches ErrPermanent, as a retry never reaches it.
var ErrGone = errors.New("push subscription gone")

type goneError struct{ error }

func (goneError) Is(target error) bool { return target == ErrGone || target == ErrPermanent }

func (e goneError) Unwrap() error { return e.error }

// Push sends one encrypted Web Push message to sub. Errors name the endpoint's host only: its path is a
// capability for the browser.
func (s *Sender) Push(ctx context.Context, sub webpush.Subscription, payload []byte, urgency string) error {
	if s.VAPID == nil {
		return errors.New("web push: no VAPID key")
	}
	if !slices.Contains([]string{UrgencyVeryLow, UrgencyLow, UrgencyNormal, UrgencyHigh}, urgency) {
		return fmt.Errorf("web push: unknown urgency %q", urgency)
	}
	u, err := url.Parse(sub.Endpoint)
	if err != nil || u.Host == "" {
		return permanentError{errors.New("web push: invalid endpoint")}
	}
	label := "POST " + u.Host
	if err := netguard.CheckScheme(u.Scheme, u.Hostname(), s.HTTPHosts); err != nil {
		return permanentError{fmt.Errorf("%s: %w", label, err)}
	}
	body, err := webpush.Encrypt(payload, sub)
	if err != nil {
		return permanentError{fmt.Errorf("web push: %w", err)}
	}
	auth, err := s.VAPID.Authorization(sub.Endpoint, s.VAPIDSubject, s.now())
	if err != nil {
		return fmt.Errorf("web push: %w", err)
	}
	hdr := http.Header{}
	hdr.Set("Authorization", auth)
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("Content-Encoding", "aes128gcm")
	hdr.Set("TTL", strconv.Itoa(PushTTL))
	hdr.Set("Urgency", urgency)
	code, _, err := s.post(ctx, SendTimeout, label, sub.Endpoint, body, hdr)
	if err != nil {
		return err
	}
	switch {
	case code >= 200 && code <= 299:
		return nil
	case code == http.StatusNotFound || code == http.StatusGone:
		return goneError{statusError(label, code, "")}
	case code == http.StatusBadRequest || code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusRequestEntityTooLarge:
		return permanentError{statusError(label, code, "")}
	}
	return statusError(label, code, "")
}

// AllDevices is the web_push devices value that targets every device of the owner, later ones included.
const AllDevices = "all"

// WebPushConfig is the config of a web_push channel. Devices is AllDevices or a list of the owner's device
// ids; stored, a list lives in channel_push_devices and Devices is left out.
type WebPushConfig struct {
	Devices any  `json:"devices,omitempty"`
	Silent  bool `json:"silent"`
}

// PushUrgency is low for a silent channel, high for an event of a high priority rule, normal otherwise.
func PushUrgency(silent bool, r *Rule) string {
	switch {
	case silent:
		return UrgencyLow
	case r != nil && r.Priority == "high":
		return UrgencyHigh
	}
	return UrgencyNormal
}

// PushMessage is the JSON the service worker gets.
type PushMessage struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
	Tag    string `json:"tag"`
	Silent bool   `json:"silent"`
}

// PushPayload is the message of d for the service worker, cut to fit one push record: body first, then
// title, on rune boundaries. url is the gedour path the notification opens.
func PushPayload(d Delivery, silent bool) []byte {
	msg := Format(d)
	m := PushMessage{Title: msg.Title, Body: msg.Detail, URL: "/inbox", Tag: "gedour-test", Silent: silent}
	if d.Event.ID != 0 {
		m.URL = "/inbox?event=" + strconv.FormatInt(d.Event.ID, 10)
		m.Tag = "gedour-event-" + strconv.FormatInt(d.Event.ID, 10)
	}
	return fitPush(m)
}

// fitPush encodes m cut to one push record: body first, then title, on rune boundaries.
func fitPush(m PushMessage) []byte {
	for {
		b := pushJSON(m)
		over := len(b) - webpush.MaxPayload
		switch {
		case over <= 0:
			return b
		case m.Body != "":
			m.Body = cutBytes(m.Body, len(m.Body)-over)
		default:
			m.Title = cutBytes(m.Title, len(m.Title)-over)
		}
	}
}

func pushJSON(m PushMessage) []byte {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(m) // strings and a bool always encode
	return []byte(strings.TrimSuffix(b.String(), "\n"))
}

// cutBytes keeps at most n bytes of s, dropping a rune the cut split.
func cutBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	return strings.ToValidUTF8(s[:min(n, len(s))], "")
}
