package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gedour/gedour/pkg/collect"
)

// incompressible is n hex characters PostgreSQL cannot shrink: its rows pay their full size in the index.
func incompressible(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		sum := sha256.Sum256(fmt.Appendf(nil, "%d", i))
		b.WriteString(hex.EncodeToString(sum[:]))
	}
	return b.String()[:n]
}

// A change to many long labels used to exceed the btree row limit on every retry, leaving the watch stuck.
func TestManyLabelsDoNotStickTheWatch(t *testing.T) {
	for _, n := range []int{60, 100} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			e := newEnv(t)
			var issue atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/comments") {
					_, _ = w.Write([]byte(`[]`))
					return
				}
				_, _ = w.Write(issue.Load().([]byte))
			}))
			defer srv.Close()
			set := func(state string, labels []string) {
				t.Helper()
				l := make([]map[string]string, len(labels))
				for i, name := range labels {
					l[i] = map[string]string{"name": name}
				}
				body, err := json.Marshal(map[string]any{"number": 1, "state": state, "title": "t", "labels": l, "html_url": "https://github.com/o/r/issues/1"})
				if err != nil {
					t.Fatal(err)
				}
				issue.Store(body)
			}
			id := e.addWatch(t, "github_issue", fmt.Sprintf(`{"repo":"o/r","number":1,"api_url":%q}`, srv.URL), "1m")
			w := e.worker(t, e.pool, collect.Collectors())
			k := 0
			run := func() {
				t.Helper()
				k++
				e.clock.set(t0.Add(time.Duration(k) * time.Hour))
				if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET next_run_at = $2 WHERE id = $1`, id, e.clock.now()); err != nil {
					t.Fatal(err)
				}
				tick(t, w)
				if r := e.row(t, id); *r.status != "ok" {
					t.Fatalf("run %d: %s %v", k, *r.status, *r.lastErr)
				}
			}
			set("open", []string{})
			run()
			labels := make([]string, n)
			text := incompressible(50 * n)
			for i := range labels {
				labels[i] = text[i*50 : (i+1)*50]
			}
			set("open", labels)
			run()
			if got := e.count(t, `SELECT count(*) FROM events WHERE watch_id = $1 AND type = 'issue.labels_changed'`, id); got != 1 {
				t.Fatalf("labels_changed events: %d", got)
			}
			set("closed", labels)
			run()
			if got := e.count(t, `SELECT count(*) FROM events WHERE watch_id = $1 AND type = 'issue.state_changed'`, id); got != 1 {
				t.Fatalf("the state did not advance past the long key: %d state_changed events", got)
			}
		})
	}
}

// 001-AC9: a replay collapses at the limit and past it, and on a key stored raw before keys were hashed.
func TestDedupKeyLength(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	insert := func(t *testing.T, id, subject string) []string {
		t.Helper()
		var got []string
		err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
			var err error
			got, err = insertEvents(ctx, tx, id, 7, []collect.Event{{Type: "t", Subject: subject, Payload: map[string]any{}}}, t0)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	key := func(subject string) string { return "t:" + subject + ":7" }
	sized := func(n int) string { return incompressible(n - len(key(""))) }
	stored := func(t *testing.T, id string) string {
		t.Helper()
		var k string
		if err := e.pool.QueryRow(ctx, `SELECT dedup_key FROM events WHERE watch_id = $1`, id).Scan(&k); err != nil {
			t.Fatal(err)
		}
		return k
	}
	t.Run("at the limit stays raw", func(t *testing.T) {
		id := e.addWatch(t, "github_repo", `{"repo":"a/b"}`, "1m")
		s := sized(maxDedupKey)
		if got := insert(t, id, s); len(got) != 1 {
			t.Fatalf("inserted %v", got)
		}
		if k := stored(t, id); k != key(s) {
			t.Fatalf("stored %.40q (%d bytes)", k, len(k))
		}
		if got := insert(t, id, s); len(got) != 0 {
			t.Fatalf("replay inserted %v", got)
		}
	})
	t.Run("one byte over is hashed", func(t *testing.T) {
		id := e.addWatch(t, "github_repo", `{"repo":"a/b"}`, "1m")
		s := sized(maxDedupKey + 1)
		if got := insert(t, id, s); len(got) != 1 {
			t.Fatalf("inserted %v", got)
		}
		sum := sha256.Sum256([]byte(key(s)))
		if k, want := stored(t, id), "sha256:"+hex.EncodeToString(sum[:]); k != want {
			t.Fatalf("stored %.40q, want %s", k, want)
		}
		if got := insert(t, id, s); len(got) != 0 {
			t.Fatalf("replay inserted %v", got)
		}
		if got := insert(t, id, s[:len(s)-1]+"x"); len(got) != 1 {
			t.Fatalf("another long key collapsed: %v", got)
		}
	})
	// The largest key an index row took before hashing, and a repetitive one PostgreSQL compresses into the row.
	for name, s := range map[string]string{"stored raw near the limit": sized(2650), "stored raw compressed": strings.Repeat("ab", 10_000)} {
		t.Run(name, func(t *testing.T) {
			id := e.addWatch(t, "github_repo", `{"repo":"a/b"}`, "1m")
			if _, err := e.pool.Exec(ctx, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', $2)`, id, key(s)); err != nil {
				t.Fatal(err)
			}
			if got := insert(t, id, s); len(got) != 0 {
				t.Fatalf("replay inserted %v", got)
			}
			if k := stored(t, id); k != key(s) {
				t.Fatalf("stored key changed to %.40q", k)
			}
		})
	}
}
