package server

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/gedour/gedour/internal/testupstream"
	"github.com/gedour/gedour/pkg/route"
	"github.com/gedour/gedour/pkg/secret"
)

// 012-AC13, 012-AC14, 012-AC11: a channel config with delivery, at or tz is a 400; a rule carries the digest,
// refused with high priority whether set on create or by an edit; "Send test" sends one event, no sample digest;
// the deliveries list filters on skipped and shows its reason, the channel's last status reads skipped.
func TestDigestAPI(t *testing.T) {
	a := newDevApp(t)
	up := testupstream.New()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	a.sender.TelegramURL = srv.URL + "/tg"
	alice := a.signIn(t, "alice")
	var ch struct {
		ID                 string  `json:"id"`
		LastDeliveryStatus *string `json:"last_delivery_status"`
	}
	tg := map[string]any{"name": "Chat", "type": "telegram", "secret": "123456:" + strings.Repeat("a", 30), "config": map[string]any{"chat_id": "-100123"}}
	for _, key := range []string{"delivery", "at", "tz"} {
		bad := maps.Clone(tg)
		bad["config"] = map[string]any{"chat_id": "-100123", key: "digest"}
		a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", bad, http.StatusBadRequest, nil)
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", tg, http.StatusCreated, &ch)
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/channels/"+ch.ID, map[string]any{"config": map[string]any{"chat_id": "-100123", "delivery": "immediate"}},
		http.StatusBadRequest, nil)

	type rule struct {
		ID       string   `json:"id"`
		Delivery string   `json:"delivery"`
		At       []string `json:"at"`
		TZ       string   `json:"tz"`
	}
	var r rule
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "plain", "channel_ids": []string{ch.ID}}, http.StatusCreated, &r)
	if r.Delivery != "immediate" || len(r.At) != 0 || r.TZ != "" {
		t.Errorf("default rule %+v, want immediate", r)
	}
	for name, body := range map[string]map[string]any{
		"no at":        {"name": "x", "delivery": "digest"},
		"bad time":     {"name": "x", "delivery": "digest", "at": []string{"9h"}},
		"unknown tz":   {"name": "x", "delivery": "digest", "at": []string{"09:00"}, "tz": "Mars/Base"},
		"unknown mode": {"name": "x", "delivery": "weekly"},
		"high":         {"name": "x", "delivery": "digest", "at": []string{"09:00"}, "priority": "high"},
	} {
		t.Run(name, func(t *testing.T) {
			a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", body, http.StatusBadRequest, nil)
		})
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "digest", "channel_ids": []string{ch.ID},
		"delivery": "digest", "at": []string{"18:00", "09:00"}, "tz": "Europe/Paris"}, http.StatusCreated, &r)
	if r.Delivery != "digest" || strings.Join(r.At, ",") != "09:00,18:00" || r.TZ != "Europe/Paris" {
		t.Errorf("digest rule %+v", r)
	}
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/rules/"+r.ID, map[string]any{"priority": "high"}, http.StatusBadRequest, nil)
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/rules/"+r.ID, map[string]any{"at": []string{"07:00"}}, http.StatusOK, &r)
	if r.Delivery != "digest" || strings.Join(r.At, ",") != "07:00" || r.TZ != "Europe/Paris" {
		t.Errorf("after an at edit %+v, want the zone kept", r)
	}
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/rules/"+r.ID, map[string]any{"delivery": "immediate", "priority": "high"}, http.StatusOK, &r)
	if r.Delivery != "immediate" || len(r.At) != 0 || r.TZ != "" {
		t.Errorf("after immediate %+v, want the schedule cleared", r)
	}

	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels/"+ch.ID+"/test", nil, http.StatusOK, &res)
	got := up.Received()
	if !res.OK || len(got) != 1 {
		t.Fatalf("test: %+v, received %d", res, len(got))
	}
	if strings.Contains(got[0].Body, "unread event") {
		t.Errorf("test sent a sample digest: %s", got[0].Body)
	}

	var w string
	if err := a.pool.QueryRow(t.Context(), `INSERT INTO watches (owner_id, name, kind, config) VALUES ($1, 'o/r', 'github_repo', '{}') RETURNING id::text`, alice.id).Scan(&w); err != nil {
		t.Fatal(err)
	}
	a.exec(t, `WITH e AS (INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 'release.published', '{"tag": "v1"}', 'k') RETURNING id)
		INSERT INTO deliveries (event_id, channel_id, status, last_error, digest) SELECT id, $2, 'skipped', 'not sent: read in the inbox before the digest', true FROM e`, w, ch.ID)
	var list struct {
		Items []struct {
			Status    string  `json:"status"`
			LastError *string `json:"last_error"`
		} `json:"items"`
	}
	a.mustCall(t, alice, http.MethodGet, "/api/v1/deliveries?status=skipped", nil, http.StatusOK, &list)
	if len(list.Items) != 1 || list.Items[0].Status != "skipped" || list.Items[0].LastError == nil || !strings.Contains(*list.Items[0].LastError, "read in the inbox") {
		t.Fatalf("skipped deliveries %+v", list.Items)
	}
	a.mustCall(t, alice, http.MethodGet, "/api/v1/channels/"+ch.ID, nil, http.StatusOK, &ch)
	if ch.LastDeliveryStatus == nil || *ch.LastDeliveryStatus != "skipped" {
		t.Errorf("last delivery status %v", ch.LastDeliveryStatus)
	}
}

// 012-AC10: a digest claim never waits on rows a purge holds. The purge parks on the watch lock after its rule
// delete locked the digest's rows (rule_id SET NULL), the claim runs, then the purge deletes the channel: had
// the claim waited on those rows under its channel lock, the two would deadlock (failOnDeadlockRetry).
func TestPurgeRacingDigestClaim(t *testing.T) {
	a := newDevApp(t)
	ctx := context.Background()
	admin, dave := a.signIn(t, "admin"), a.signIn(t, "dave")
	a.exec(t, `UPDATE sessions SET is_admin = true WHERE user_id = $1`, admin.id)
	var w watchJSON
	a.mustCall(t, dave, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	var ch, rule string
	if err := a.pool.QueryRow(ctx, `INSERT INTO channels (owner_id, name, type, config) VALUES ($1, 'd', 'telegram',
		'{"chat_id": "-100123"}') RETURNING id::text`, dave.id).Scan(&ch); err != nil {
		t.Fatal(err)
	}
	if err := a.pool.QueryRow(ctx, `INSERT INTO rules (owner_id, name, delivery, digest_at, digest_tz, all_watches) VALUES ($1, 'r', 'digest', '{07:00}', 'UTC', true) RETURNING id::text`, dave.id).Scan(&rule); err != nil {
		t.Fatal(err)
	}
	a.exec(t, `WITH e AS (INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 'release.published', '{"tag": "v1"}', 'k') RETURNING id)
		INSERT INTO deliveries (event_id, rule_id, channel_id, digest) SELECT id, $2, $3, true FROM e`, w.ID, rule, ch)
	a.mustCall(t, admin, http.MethodPost, "/api/v1/admin/users/"+dave.id+"/disable", nil, http.StatusNoContent, nil)

	hold, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR KEY SHARE`, w.ID); err != nil {
		t.Fatal(err)
	}
	purge := a.callAsync(admin, http.MethodPost, "/api/v1/admin/users/"+dave.id+"/purge", nil)
	a.waitQueryLocked(t, "%FROM watches WHERE owner_id = $1 ORDER BY id FOR UPDATE%")

	box, _ := secret.New(testKey)
	d := route.NewDeliverer(a.pool, zap.NewNop(), box, a.sender, nil, route.NewMetrics(prometheus.NewRegistry()), time.Hour, 4, time.Second)
	claimed := make(chan error, 1)
	go func() {
		_, err := d.DeliverOnce(ctx)
		claimed <- err
	}()
	select {
	case err := <-claimed:
		if err != nil {
			t.Errorf("claim: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the digest claim waited on the purge's rows")
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-purge; code != http.StatusNoContent {
		t.Fatalf("purge: %d, want 204", code)
	}
}
