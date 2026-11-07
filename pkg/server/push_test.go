package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gedour/gedour/internal/testupstream"
	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/notify"
	"github.com/gedour/gedour/pkg/notify/webpush"
	"github.com/gedour/gedour/pkg/route"
)

type deviceJSON struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	UserAgent  string  `json:"user_agent"`
	LastPushAt *string `json:"last_push_at"`
}

func pushBody(endpoint, name string) map[string]any {
	return map[string]any{"endpoint": endpoint, "name": name, "user_agent": "Firefox/140",
		"keys": map[string]any{"p256dh": testupstream.PushP256DH, "auth": testupstream.PushAuth}}
}

// newPushApp is a dev app with a VAPID key, and a fake push service's base URL.
func newPushApp(t *testing.T) (*devApp, *testupstream.Server, string) {
	t.Helper()
	a := newDevApp(t)
	k, err := webpush.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	a.sender.VAPID, a.sender.HTTPHosts = k, []string{"127.0.0.1"}
	up := testupstream.New()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	return a, up, srv.URL
}

func (a *devApp) devices(t *testing.T, u apiUser) []deviceJSON {
	t.Helper()
	var out struct {
		Items []deviceJSON `json:"items"`
	}
	a.mustCall(t, u, http.MethodGet, "/api/v1/push/devices", nil, http.StatusOK, &out)
	return out.Items
}

// 010-AC1: a valid subscription registers; a non-https endpoint, a private address, bad keys or a blank name are 400.
func TestPushDeviceRegister(t *testing.T) {
	a, _, base := newPushApp(t)
	alice := a.signIn(t, "alice")
	var d deviceJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/push/devices", pushBody(base+"/push/a", " Laptop "), http.StatusCreated, &d)
	if d.Name != "Laptop" || d.UserAgent != "Firefox/140" || d.LastPushAt != nil {
		t.Fatalf("%+v", d)
	}
	keys := func(p256dh, auth string) map[string]any {
		b := pushBody(base+"/push/b", "x")
		b["keys"] = map[string]any{"p256dh": p256dh, "auth": auth}
		return b
	}
	notOnCurve := base64.RawURLEncoding.EncodeToString(append([]byte{4}, make([]byte, 64)...))
	for _, tt := range []struct {
		name string
		body map[string]any
		locs []string
	}{
		{"plain http", pushBody("http://push.example/x", "x"), []string{"body.endpoint"}},
		{"private address", pushBody("https://10.1.2.3/push/x", "x"), []string{"body.endpoint"}},
		{"private v6", pushBody("https://[fd00::1]/push/x", "x"), []string{"body.endpoint"}},
		{"relative", pushBody("/push/x", "x"), []string{"body.endpoint"}},
		{"credentials", pushBody("https://u:p@push.example/x", "x"), []string{"body.endpoint"}}, //nolint:gosec // the refused shape itself
		{"short p256dh", keys(testupstream.PushP256DH[:40], testupstream.PushAuth), []string{"body.keys.p256dh"}},
		{"off curve", keys(notOnCurve, testupstream.PushAuth), []string{"body.keys.p256dh"}},
		{"not base64", keys("!!!", testupstream.PushAuth), []string{"body.keys.p256dh"}},
		{"short auth", keys(testupstream.PushP256DH, testupstream.PushAuth[:10]), []string{"body.keys.auth"}},
		{"blank name", pushBody(base+"/push/c", "  "), []string{"body.name"}},
		{"NUL in name", pushBody(base+"/push/c", "a\x00b"), []string{"body.name"}},
	} {
		p := a.problemOf(t, alice, http.MethodPost, "/api/v1/push/devices", tt.body, http.StatusBadRequest)
		if got := p.locations(); !slices.Equal(got, tt.locs) {
			t.Errorf("%s: locations %v, want %v", tt.name, got, tt.locs)
		}
	}
	// Padded base64url, as some browsers give, is fine.
	a.mustCall(t, alice, http.MethodPost, "/api/v1/push/devices", keys(testupstream.PushP256DH+"=", testupstream.PushAuth+"=="), http.StatusCreated, nil)
	if n := len(a.devices(t, alice)); n != 2 {
		t.Fatalf("%d devices", n)
	}
	for i := 2; i < route.MaxDevices; i++ {
		a.mustCall(t, alice, http.MethodPost, "/api/v1/push/devices", pushBody(fmt.Sprintf("%s/push/n%d", base, i), "n"), http.StatusCreated, nil)
	}
	if p := a.problemOf(t, alice, http.MethodPost, "/api/v1/push/devices", pushBody(base+"/push/over", "n"), http.StatusConflict); !strings.Contains(p.Detail, "20 push devices") {
		t.Errorf("cap message: %q", p.Detail)
	}
	// At the cap, a known endpoint still updates its device.
	a.mustCall(t, alice, http.MethodPost, "/api/v1/push/devices", pushBody(base+"/push/a", "Laptop 2"), http.StatusCreated, nil)
}

// 010-AC2, 010-AC3: a user sees, renames and removes only their own devices; the same endpoint again
// updates its device; from another user it moves the device and drops it from the first user's lists.
func TestPushDeviceOwnership(t *testing.T) {
	a, _, base := newPushApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var d, again deviceJSON
	mine := func(name string) map[string]any {
		b := pushBody(base+"/push/a", name)
		b["user_agent"] = "AliceAgent/1"
		return b
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/push/devices", mine("Laptop"), http.StatusCreated, &d)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/push/devices", mine("Work laptop"), http.StatusCreated, &again)
	if again.ID != d.ID || again.Name != "Work laptop" || len(a.devices(t, alice)) != 1 {
		t.Fatalf("re-register: %+v", again)
	}
	if len(a.devices(t, bob)) != 0 {
		t.Fatal("bob sees alice's device")
	}
	for _, m := range []string{http.MethodPatch, http.MethodDelete} {
		a.mustCall(t, bob, m, "/api/v1/push/devices/"+d.ID, map[string]any{"name": "mine"}, http.StatusNotFound, nil)
	}
	var renamed deviceJSON
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/push/devices/"+d.ID, map[string]any{"name": " Phone "}, http.StatusOK, &renamed)
	if renamed.Name != "Phone" {
		t.Fatal(renamed)
	}
	for _, name := range []string{" ", "a\x00b"} {
		if p := a.problemOf(t, alice, http.MethodPatch, "/api/v1/push/devices/"+d.ID, map[string]any{"name": name}, http.StatusBadRequest); !slices.Equal(p.locations(), []string{"body.name"}) {
			t.Errorf("rename %q: %v", name, p.locations())
		}
	}

	var ch channelJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Push", "type": "web_push", "config": map[string]any{"devices": []string{d.ID}}}, http.StatusCreated, &ch)
	a.exec(t, `UPDATE push_devices SET last_push_at = now(), created_at = now() - interval '1 day' WHERE id = $1`, d.ID)
	var moved deviceJSON
	a.mustCall(t, bob, http.MethodPost, "/api/v1/push/devices", pushBody(base+"/push/a", "Bob's"), http.StatusCreated, &moved)
	if moved.ID != d.ID || len(a.devices(t, alice)) != 0 || len(a.devices(t, bob)) != 1 {
		t.Fatalf("move: %+v", moved)
	}
	// The new owner starts afresh: nothing of the device's life with alice.
	if moved.LastPushAt != nil || a.countRows(t, `SELECT count(*) FROM push_devices WHERE id = $1 AND created_at > now() - interval '1 minute'`, d.ID) != 1 {
		t.Fatalf("moved device keeps alice's history: %+v", moved)
	}
	a.mustCall(t, alice, http.MethodGet, "/api/v1/channels/"+ch.ID, nil, http.StatusOK, &ch)
	if fmt.Sprint(ch.Config["devices"]) != "[]" {
		t.Fatalf("alice's list still names the moved device: %v", ch.Config)
	}
	// One entry per owner: the old owner's has no actor and names only them, the new owner's only them.
	if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE action = 'push_device.moved' AND actor_user_id IS NULL
		AND before->>'owner_id' = $1 AND after->>'owner_id' IS NULL`, alice.id); n != 1 {
		t.Fatalf("%d old-owner move entries", n)
	}
	if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE action = 'push_device.moved' AND actor_user_id = $1
		AND before->>'owner_id' IS NULL AND after->>'owner_id' = $1::text`, bob.id); n != 1 {
		t.Fatalf("%d new-owner move entries", n)
	}
	// An admin acting as bob takes over another of alice's browsers: alice's entry names neither, and shares the request id.
	carol := a.signIn(t, "carol")
	a.exec(t, `UPDATE sessions SET is_admin = true WHERE user_id = $1`, carol.id)
	var other deviceJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/push/devices", func() map[string]any { b := mine("Tablet"); b["endpoint"] = base + "/push/tablet"; return b }(), http.StatusCreated, &other)
	a.mustCall(t, carol, http.MethodPost, "/api/v1/admin/users/"+bob.id+"/act-as", nil, http.StatusNoContent, nil)
	a.mustCall(t, carol, http.MethodPost, "/api/v1/push/devices", pushBody(base+"/push/tablet", "Bob's tablet"), http.StatusCreated, nil)
	a.mustCall(t, carol, http.MethodDelete, "/api/v1/admin/act-as", nil, http.StatusNoContent, nil)
	if n := a.countRows(t, `SELECT count(*) FROM audit_log o JOIN audit_log b ON b.target_id = o.target_id AND b.action = o.action AND b.id <> o.id
		WHERE o.action = 'push_device.moved' AND o.target_id = $1 AND o.before->>'owner_id' = $2::text
			AND o.actor_user_id IS NULL AND o.on_behalf_of IS NULL AND o.api_key_id IS NULL
			AND b.actor_user_id = $3 AND b.on_behalf_of = $4 AND o.request_id = b.request_id AND o.request_id <> ''`, other.ID, alice.id, carol.id, bob.id); n != 1 {
		t.Fatalf("%d clean old-owner entries for the acting move", n)
	}
	// 010-AC16: the move shows to both owners; the new one reads nothing of the old one's device.
	if acts := a.auditActions(t, alice, d.ID); !slices.Contains(acts, "push_device.moved") {
		t.Errorf("alice's audit: %v", acts)
	}
	if acts := a.auditActions(t, bob, d.ID); !slices.Equal(acts, []string{"push_device.moved"}) {
		t.Errorf("bob's audit: %v", acts)
	}
	_, raw := a.call(t, bob, http.MethodGet, "/api/v1/audit?target_type=push_device", nil)
	for _, leak := range []string{"AliceAgent", "Laptop", "laptop", "Phone"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("bob's audit carries alice's %q: %s", leak, raw)
		}
	}
	// Neither owner learns who the other is, in the log or in its actor list.
	for _, tt := range []struct {
		viewer apiUser
		leaks  []string
	}{
		{alice, []string{bob.id, bob.email, `"bob"`, carol.id, carol.email, `"carol"`}},
		{bob, []string{alice.id, alice.email, `"alice"`}},
	} {
		for _, path := range []string{"/api/v1/audit", "/api/v1/audit/actors"} {
			_, body := a.call(t, tt.viewer, http.MethodGet, path, nil)
			for _, leak := range tt.leaks {
				if strings.Contains(string(body), leak) {
					t.Errorf("%s of %s carries %q: %s", path, tt.viewer.email, leak, body)
				}
			}
		}
	}
	a.mustCall(t, bob, http.MethodDelete, "/api/v1/push/devices/"+d.ID, nil, http.StatusNoContent, nil)
	a.mustCall(t, bob, http.MethodDelete, "/api/v1/push/devices/"+d.ID, nil, http.StatusNotFound, nil)
}

// 010-AC5, 010-AC16: a web_push channel lists only the owner's devices, loses a removed one, has no secret;
// the audit log and the API output never carry an endpoint or keys.
func TestPushChannels(t *testing.T) {
	a, up, base := newPushApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var d1, d2, theirs deviceJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/push/devices", pushBody(base+"/push/one", "One"), http.StatusCreated, &d1)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/push/devices", pushBody(base+"/push/gone1", "Two"), http.StatusCreated, &d2)
	a.mustCall(t, bob, http.MethodPost, "/api/v1/push/devices", pushBody(base+"/push/bob", "Bob"), http.StatusCreated, &theirs)
	push := func(cfg map[string]any) map[string]any {
		return map[string]any{"name": "Push", "type": "web_push", "config": cfg}
	}
	for _, tt := range []struct {
		name string
		body map[string]any
		locs []string
	}{
		{"another user's device", push(map[string]any{"devices": []string{d1.ID, theirs.ID}}), []string{"body.config.devices[1]"}},
		{"unknown id", push(map[string]any{"devices": []string{"6f1c1a43-6d5b-4c3a-9b8f-0e1f2a3b4c5d"}}), []string{"body.config.devices[0]"}},
		{"not an id", push(map[string]any{"devices": []any{"x", 3}}), []string{"body.config.devices[0]", "body.config.devices[1]"}},
		{"empty list", push(map[string]any{"devices": []string{}}), []string{"body.config.devices"}},
		{"other word", push(map[string]any{"devices": "some"}), []string{"body.config.devices"}},
		{"missing", push(map[string]any{"silent": true}), []string{"body.config.devices"}},
		{"wrong type", push(map[string]any{"devices": "all", "silent": "yes"}), []string{"body.config"}},
		{"unknown key", push(map[string]any{"devices": "all", "url": "x"}), []string{"body.config"}},
		{"secret", map[string]any{"name": "Push", "type": "web_push", "config": map[string]any{"devices": "all"}, "secret": "s"}, []string{"body.secret"}},
	} {
		p := a.problemOf(t, alice, http.MethodPost, "/api/v1/channels", tt.body, http.StatusBadRequest)
		if got := p.locations(); !slices.Equal(got, tt.locs) {
			t.Errorf("%s: locations %v, want %v", tt.name, got, tt.locs)
		}
	}
	var listed, all channelJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", push(map[string]any{"devices": []string{d1.ID, d2.ID, d1.ID}, "silent": true}), http.StatusCreated, &listed)
	if fmt.Sprint(listed.Config["devices"]) != fmt.Sprint(sorted(d1.ID, d2.ID)) || listed.Config["silent"] != true || listed.Last4 != "" {
		t.Fatalf("listed: %+v", listed)
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "All", "type": "web_push", "config": map[string]any{"devices": "all"}}, http.StatusCreated, &all)
	if all.Config["devices"] != "all" || all.Config["silent"] != false {
		t.Fatalf("all: %+v", all)
	}
	// A patch without config keeps the list; one with config replaces it.
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/channels/"+listed.ID, map[string]any{"name": "Picked"}, http.StatusOK, &listed)
	if fmt.Sprint(listed.Config["devices"]) != fmt.Sprint(sorted(d1.ID, d2.ID)) {
		t.Fatalf("rename lost the list: %v", listed.Config)
	}
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/channels/"+listed.ID, map[string]any{"config": map[string]any{"devices": []string{theirs.ID}}}, http.StatusBadRequest, nil)
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/channels/"+all.ID, map[string]any{"config": map[string]any{"devices": []string{d1.ID}}}, http.StatusOK, &all)
	if fmt.Sprint(all.Config["devices"]) != fmt.Sprint([]string{d1.ID}) {
		t.Fatalf("all to list: %v", all.Config)
	}
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/channels/"+all.ID, map[string]any{"config": map[string]any{"devices": "all"}}, http.StatusOK, &all)
	if all.Config["devices"] != "all" || a.countRows(t, `SELECT count(*) FROM channel_push_devices WHERE channel_id = $1`, all.ID) != 0 {
		t.Fatalf("list to all: %v", all.Config)
	}

	// "Send test": the gone device goes, the other accepts.
	var res struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Accepted *int   `json:"accepted"`
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels/"+listed.ID+"/test", nil, http.StatusOK, &res)
	if !res.OK || res.Accepted == nil || *res.Accepted != 1 || len(a.devices(t, alice)) != 1 {
		t.Fatalf("test: %+v", res)
	}
	if got := up.Received(); len(got) != 2 || !strings.Contains(got[0].Body+got[1].Body, `"silent":true`) {
		t.Fatalf("received %+v", got)
	}
	a.mustCall(t, alice, http.MethodGet, "/api/v1/channels/"+listed.ID, nil, http.StatusOK, &listed)
	if fmt.Sprint(listed.Config["devices"]) != fmt.Sprint([]string{d1.ID}) {
		t.Fatalf("gone device still listed: %v", listed.Config)
	}
	a.mustCall(t, alice, http.MethodDelete, "/api/v1/push/devices/"+d1.ID, nil, http.StatusNoContent, nil)
	a.mustCall(t, alice, http.MethodGet, "/api/v1/channels/"+listed.ID, nil, http.StatusOK, &listed)
	if fmt.Sprint(listed.Config["devices"]) != "[]" {
		t.Fatalf("removed device still listed: %v", listed.Config)
	}
	a.mustCall(t, alice, http.MethodDelete, "/api/v1/channels/"+listed.ID, nil, http.StatusNoContent, nil)

	// 010-AC8: the owner reads the removal of a gone device in their audit log.
	if acts := a.auditActions(t, alice, d2.ID); !slices.Contains(acts, "push_device.gone") {
		t.Errorf("alice's audit of the gone device: %v", acts)
	}
	if acts := a.auditActions(t, bob, d2.ID); len(acts) != 0 {
		t.Errorf("bob sees alice's device: %v", acts)
	}
	// 010-AC16: device and web_push channel changes are audited, without an endpoint or a key.
	for _, action := range []string{"push_device.registered", "push_device.deleted", "push_device.gone", "channel.created", "channel.updated", "channel.deleted"} {
		if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE action = $1`, action); n == 0 {
			t.Errorf("no %s entry", action)
		}
	}
	_, chans := a.call(t, alice, http.MethodGet, "/api/v1/channels", nil)
	_, devs := a.call(t, bob, http.MethodGet, "/api/v1/push/devices", nil)
	for _, leak := range []string{"/push/", testupstream.PushP256DH, testupstream.PushAuth, "p256dh", "endpoint"} {
		if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE COALESCE(before::text, '') || COALESCE(after::text, '') LIKE '%' || $1 || '%'`, leak); n != 0 {
			t.Errorf("%d audit entries carry %q", n, leak)
		}
		if strings.Contains(string(chans)+string(devs), leak) {
			t.Errorf("API output carries %q", leak)
		}
	}
}

func sorted(ids ...string) []string {
	slices.Sort(ids)
	return ids
}

// The VAPID key is served to signed-in users; with no key Web Push is off: 503, and the type is unavailable.
func TestPushKey(t *testing.T) {
	a, _, _ := newPushApp(t)
	alice := a.signIn(t, "alice")
	var k struct {
		PublicKey string `json:"public_key"`
	}
	a.mustCall(t, alice, http.MethodGet, "/api/v1/push/key", nil, http.StatusOK, &k)
	if k.PublicKey != a.sender.VAPID.PublicKey() {
		t.Fatal(k)
	}
	a.sender.VAPID = nil
	p := a.problemOf(t, alice, http.MethodGet, "/api/v1/push/key", nil, http.StatusServiceUnavailable)
	if !strings.Contains(p.Detail, "Web Push is off") {
		t.Fatal(p.Detail)
	}
	var types struct {
		Items []struct {
			Type      string `json:"type"`
			Available bool   `json:"available"`
			Hint      string `json:"hint"`
		} `json:"items"`
	}
	a.mustCall(t, alice, http.MethodGet, "/api/v1/channel-types", nil, http.StatusOK, &types)
	last := types.Items[len(types.Items)-1]
	if last.Type != "web_push" || last.Available || last.Hint == "" {
		t.Fatalf("%+v", last)
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Push", "type": "web_push", "config": map[string]any{"devices": "all"}}, http.StatusBadRequest, nil)
}

// auditActions lists the actions on target that u's audit log shows, newest first.
func (a *devApp) auditActions(t *testing.T, u apiUser, target string) []string {
	t.Helper()
	var out struct {
		Items []struct {
			Action   string `json:"action"`
			TargetID string `json:"target_id"`
		} `json:"items"`
	}
	a.mustCall(t, u, http.MethodGet, "/api/v1/audit?target_type=push_device", nil, http.StatusOK, &out)
	acts := []string{}
	for _, it := range out.Items {
		if it.TargetID == target {
			acts = append(acts, it.Action)
		}
	}
	return acts
}

// 010-AC1: a hostname registers, whatever it resolves to; the dial guard refuses it at send unless its range is allowed.
func TestPushDeviceHostname(t *testing.T) {
	a, _, _ := newPushApp(t)
	alice := a.signIn(t, "alice")
	a.mustCall(t, alice, http.MethodPost, "/api/v1/push/devices", pushBody("https://localhost/push/x", "x"), http.StatusCreated, nil)
	sub := testupstream.PushSubscription("https://localhost:1/push/x")
	for _, tt := range []struct {
		allow   []string
		blocked bool
	}{{nil, true}, {[]string{"127.0.0.0/8", "::1/128"}, false}} {
		g, _ := netguard.New(tt.allow)
		s := *a.sender
		s.Client = g.Client(netguard.Options{Timeout: notify.SendTimeout, NoRedirects: true})
		err := s.Push(context.Background(), sub, []byte("x"), notify.UrgencyNormal)
		if err == nil || errors.Is(err, netguard.ErrBlocked) != tt.blocked {
			t.Errorf("allow %v: %v", tt.allow, err)
		}
	}
}

// At the cap, taking over another user's browser is refused like a new device; the device stays where it was.
func TestPushDeviceMoveAtCap(t *testing.T) {
	a, _, base := newPushApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var d deviceJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/push/devices", pushBody(base+"/push/shared", "Shared"), http.StatusCreated, &d)
	for i := range route.MaxDevices {
		a.mustCall(t, bob, http.MethodPost, "/api/v1/push/devices", pushBody(fmt.Sprintf("%s/push/b%d", base, i), "n"), http.StatusCreated, nil)
	}
	a.mustCall(t, bob, http.MethodPost, "/api/v1/push/devices", pushBody(base+"/push/shared", "Mine"), http.StatusConflict, nil)
	if got := a.devices(t, alice); len(got) != 1 || got[0].ID != d.ID {
		t.Fatalf("alice's devices: %+v", got)
	}
}

// Two users registering one new endpoint at once: one device, the other call is a 409, never a 500.
func TestPushDeviceRegisterRace(t *testing.T) {
	a, _, base := newPushApp(t)
	alice, bob, carol := a.signIn(t, "alice"), a.signIn(t, "bob"), a.signIn(t, "carol")
	endpoint := base + "/push/race"
	hash := sha256.Sum256([]byte(endpoint))
	// An uncommitted insert of the same endpoint holds both registrations on the unique index.
	tx, err := a.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `INSERT INTO push_devices (owner_id, name, endpoint_hash, sealed, nonce, key_version)
		VALUES ($1, 'held', $2, '', '', 1)`, carol.id, hash[:]); err != nil {
		t.Fatal(err)
	}
	first := a.callAsync(alice, http.MethodPost, "/api/v1/push/devices", pushBody(endpoint, "A"))
	second := a.callAsync(bob, http.MethodPost, "/api/v1/push/devices", pushBody(endpoint, "B"))
	a.waitLockWaiters(t, 2)
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	codes := []int{<-first, <-second}
	slices.Sort(codes)
	if !slices.Equal(codes, []int{http.StatusCreated, http.StatusConflict}) {
		t.Fatalf("codes %v", codes)
	}
	if n := a.countRows(t, `SELECT count(*) FROM push_devices WHERE endpoint_hash = $1`, hash[:]); n != 1 {
		t.Fatalf("%d devices", n)
	}
}

// A test some devices take and others refuse worked: ok, with the refusals in error.
func TestPushTestPartlyRefused(t *testing.T) {
	a, _, base := newPushApp(t)
	alice := a.signIn(t, "alice")
	a.mustCall(t, alice, http.MethodPost, "/api/v1/push/devices", pushBody(base+"/push/ok", "Ok"), http.StatusCreated, nil)
	bad := pushBody(base+"/push/bad", "Bad")
	bad["keys"] = map[string]any{"p256dh": testupstream.PushP256DH, "auth": "AAAAAAAAAAAAAAAAAAAAAA"}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/push/devices", bad, http.StatusCreated, nil)
	var ch channelJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Push", "type": "web_push", "config": map[string]any{"devices": "all"}}, http.StatusCreated, &ch)
	var res struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Accepted *int   `json:"accepted"`
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels/"+ch.ID+"/test", nil, http.StatusOK, &res)
	if !res.OK || res.Accepted == nil || *res.Accepted != 1 || !strings.HasPrefix(res.Error, "1 of 2 devices refused") || !strings.Contains(res.Error, "HTTP 400") {
		t.Fatalf("%+v", res)
	}
}
