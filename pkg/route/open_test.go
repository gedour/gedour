package route

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/gedour/gedour/pkg/notify"
)

// The vector of pkg/notify's TestReadOnOpenVector: a link valid under vectorEncKey.
const (
	vectorEncKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	vectorLink   = "42.6f1d2c3b-4a59-4e8f-9a0b-1c2d3e4f5a6b.8hMGpzkjn5WyKjTeh8UnyQ"
)

// 021-AC9: every link that does not verify, and any link on a key-less service, is the same 404 before any
// query: the service has no pool, so a query would panic.
func TestReadOnOpenRefusedBeforeQuery(t *testing.T) {
	raw, _ := base64.StdEncoding.DecodeString(vectorEncKey)
	key, err := notify.DeriveOpenKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := (&notify.Sender{OpenKey: key}).VerifyOpen(vectorLink); !ok {
		t.Fatal("setup: the vector does not verify")
	}
	other, _ := notify.DeriveOpenKey(make([]byte, 32))
	head := vectorLink[:strings.LastIndexByte(vectorLink, '.')+1]
	for _, tt := range []struct {
		key  []byte
		link string
	}{
		{key, head + "8hMGpzkjn5WyKjTeh8UnyR"},
		{key, "43" + vectorLink[2:]},
		{key, strings.Replace(vectorLink, "6f1d", "7f1d", 1)},
		{key, "x"},
		{key, ""},
		{key, vectorLink + strings.Repeat("A", 900)},
		{other, vectorLink},
		{nil, vectorLink},
	} {
		s := &Service{sender: &notify.Sender{OpenKey: tt.key}}
		in := &openInput{}
		in.Body.Link = tt.link
		out, err := s.open(context.Background(), in)
		var se huma.StatusError
		if out != nil || !errors.As(err, &se) || se.GetStatus() != 404 || err.Error() != "this link is not valid" {
			t.Errorf("%q key %v: %v %v", tt.link, tt.key != nil, out, err)
		}
	}
}
