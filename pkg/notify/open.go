package notify

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// DeriveOpenKey is the read-on-open HMAC key (021) of the instance encryption key.
func DeriveOpenKey(encryptionKey []byte) ([]byte, error) {
	k, err := hkdf.Key(sha256.New, encryptionKey, nil, "gedour read-on-open v1", 32)
	if err != nil {
		return nil, fmt.Errorf("read-on-open key: %w", err)
	}
	return k, nil
}

// openLink is "<event id>.<owner id>.<tag>"; empty without a key.
func openLink(key []byte, eventID int64, owner string) string {
	if len(key) == 0 {
		return ""
	}
	msg := strconv.FormatInt(eventID, 10) + "." + owner
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return msg + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

// VerifyOpen reads a signed link (021): the event id and owner id it binds, ok false unless its tag matches.
func (s *Sender) VerifyOpen(link string) (eventID int64, owner string, ok bool) {
	parts := strings.Split(link, ".")
	if len(s.OpenKey) == 0 || len(parts) != 3 {
		return 0, "", false
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		return 0, "", false
	}
	u, err := uuid.Parse(parts[1])
	if err != nil {
		return 0, "", false
	}
	// The whole canonical string is compared, tag undecoded: no other spelling of the id, owner or tag passes.
	if !hmac.Equal([]byte(openLink(s.OpenKey, id, u.String())), []byte(link)) {
		return 0, "", false
	}
	return id, u.String(), true
}

func readOnOpen(p *bool) bool { return p != nil && *p }

// upstream is the URL a message links to upstream: u, or its signed /r link when read_on_open is on (021).
func (s *Sender) upstream(on bool, owner string, eventID int64, u string) string {
	if !on || s.PublicURL == "" || len(s.OpenKey) == 0 || u == "" || eventID == 0 || owner == "" {
		return u
	}
	if !LinkableURL(u) {
		return ""
	}
	return strings.TrimRight(s.PublicURL, "/") + "/r#" + openLink(s.OpenKey, eventID, owner)
}

// LinkableURL reports whether u may be a read-on-open target: http(s), parsed with a host (021).
func LinkableURL(u string) bool {
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return false
	}
	p, err := url.Parse(u)
	return err == nil && p.Host != ""
}
