package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"

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
	code, _, err := s.post(ctx, label, sub.Endpoint, body, hdr)
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
