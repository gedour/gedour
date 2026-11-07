package server

import (
	"context"
	"encoding/json"
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

// 012-AC11: the deliveries list filters on skipped and shows its reason, the channel's last status reads
// skipped, and "Send test" on a digest channel sends a sample digest of two events.
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
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Digest", "type": "telegram", "secret": "123456:" + strings.Repeat("a", 30),
		"config": map[string]any{"chat_id": "-100123", "delivery": "digest", "at": []string{"09:00"}, "tz": "Europe/Paris"}}, http.StatusCreated, &ch)

	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels/"+ch.ID+"/test", nil, http.StatusOK, &res)
	got := up.Received()
	if !res.OK || len(got) != 1 {
		t.Fatalf("test: %+v, received %d", res, len(got))
	}
	var msg struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(got[0].Body), &msg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.Text, "2 unread events") || strings.Count(msg.Text, "\n• ") != 2 {
		t.Errorf("sample digest %q", msg.Text)
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
		'{"chat_id": "-100123", "delivery": "digest", "at": ["07:00"], "tz": "UTC"}') RETURNING id::text`, dave.id).Scan(&ch); err != nil {
		t.Fatal(err)
	}
	if err := a.pool.QueryRow(ctx, `INSERT INTO rules (owner_id, name) VALUES ($1, 'r') RETURNING id::text`, dave.id).Scan(&rule); err != nil {
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
