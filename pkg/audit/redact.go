package audit

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Redacted is the value that replaces a secret.
const Redacted = "[redacted]"

// Compared after lowercasing and dropping '_' / '-', so apiKey, API-KEY and api_key all match; the
// suffix check also tries the key without one trailing 's' (apiKeys, encryption_keys).
// Over-matching is accepted: a redacted harmless field costs less than a leaked secret.
var (
	secretMarkers = []string{
		"secret", "password", "passwd", "pwd", "passphrase", "passcode", "token", "jwt", "privatekey",
		"authorization", "authheader", "bearer", "cookie", "credential", "dsn",
	}
	secretSuffixes = []string{"key", "auth", "pass"}
)

func isSecretKey(k string) bool {
	k = strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(k))
	for _, cand := range []string{k, strings.TrimSuffix(k, "s")} {
		for _, m := range secretSuffixes {
			if strings.HasSuffix(cand, m) {
				return true
			}
		}
	}
	for _, m := range secretMarkers {
		if strings.Contains(k, m) {
			return true
		}
	}
	return false
}

// redactedJSON marshals v with encoding/json (its tags and MarshalJSON decide the shape, as in the API),
// then replaces every secret-looking key of the resulting tree, at any depth, by Redacted.
// A nil value, typed nil pointer included, gives nil (SQL NULL).
func redactedJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Numbers stay exact (ids, big counters) instead of going through float64.
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	if tree == nil {
		return nil, nil
	}
	return json.Marshal(redact(tree))
}

func redact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if isSecretKey(k) {
				t[k] = Redacted
			} else {
				t[k] = redact(val)
			}
		}
	case []any:
		for i, val := range t {
			t[i] = redact(val)
		}
	}
	return v
}
