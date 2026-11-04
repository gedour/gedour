package audit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/testpg"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testpg.Terminate()
	os.Exit(code)
}

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := db.Connect(ctx, testpg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func newUser(t *testing.T, pool *pgxpool.Pool, sub string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `INSERT INTO users (oidc_sub, display_name) VALUES ($1, $1) RETURNING id::text`, sub).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// 001-AC21: secrets are redacted at any depth, whatever the key's case or separator.
func TestRedaction(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, ""},
		{"flat", map[string]any{"name": "w", "password": "hunter2"}, `{"name":"w","password":"[redacted]"}`},
		{"case and separators", map[string]any{"API_KEY": "a", "apiKey": "b", "Private-Key": "c", "clientSecret": "d"},
			`{"API_KEY":"[redacted]","Private-Key":"[redacted]","apiKey":"[redacted]","clientSecret":"[redacted]"}`},
		{"nested object", map[string]any{"cfg": map[string]any{"login": map[string]any{"access_token": "t", "user": "u"}}},
			`{"cfg":{"login":{"access_token":"[redacted]","user":"u"}}}`},
		{"array of objects", []any{map[string]any{"secret": 1}, map[string]any{"ok": true}}, `[{"secret":"[redacted]"},{"ok":true}]`},
		{"secret holding an object", map[string]any{"token": map[string]any{"value": "x"}}, `{"token":"[redacted]"}`},
		{"struct with tags", struct {
			Name   string `json:"name"`
			Secret string `json:"webhook_secret"`
		}{"n", "s"}, `{"name":"n","webhook_secret":"[redacted]"}`},
		{"big number stays exact", map[string]any{"id": int64(9007199254740993)}, `{"id":9007199254740993}`},
		{"value that looks secret is kept", map[string]any{"note": "password"}, `{"note":"password"}`},
		{"auth headers and cookies", map[string]any{"headers": map[string]any{"Authorization": "Bearer x", "Set-Cookie": "a=b", "X-Bearer": "y"}},
			`{"headers":{"Authorization":"[redacted]","Set-Cookie":"[redacted]","X-Bearer":"[redacted]"}}`},
		{"credentials, passphrase, dsn", map[string]any{"credentials": []any{"u"}, "sshPassphrase": "p", "database_dsn": "postgres://h/db", "name": "n"},
			`{"credentials":"[redacted]","database_dsn":"[redacted]","name":"n","sshPassphrase":"[redacted]"}`},
		// The type's own JSON form decides what is recorded: json:"-" and MarshalJSON keep secrets out.
		{"json:\"-\" secret", account{Name: "a", PlainSecret: "s"}, `{"name":"a"}`},
		{"MarshalJSON hides a field", masked{User: "u", Hidden: "s"}, `{"user":"u"}`},
		{"nested generic tree", map[string]any{"items": []any{map[string]any{"cfg": map[string]any{"client_secret": "s", "url": "u"}}}, "list": []any{[]any{map[string]any{"token": 1}}}},
			`{"items":[{"cfg":{"client_secret":"[redacted]","url":"u"}}],"list":[[{"token":"[redacted]"}]]}`},
		{"typed nil pointer", (*account)(nil), ""},
		{"struct inside map", map[string]any{"acct": account{Name: "a", PlainSecret: "s", APIKey: "k"}}, `{"acct":{"api_key":"[redacted]","name":"a"}}`},
		// Deny-list breadth: passwd/pwd, any *key, exact "auth"; "author" is not a secret.
		{"passwd, pwd, key suffix, auth", map[string]any{"passwd": 1, "db_pwd": 2, "signing_key": 3, "privateKey": 4, "APIKEY": 5, "Auth": 6, "author": "a"},
			`{"APIKEY":"[redacted]","Auth":"[redacted]","author":"a","db_pwd":"[redacted]","passwd":"[redacted]","privateKey":"[redacted]","signing_key":"[redacted]"}`},
		{"plural keys, pem, jwt, passcode, auth headers", map[string]any{
			"apiKeys": 1, "api_keys": 2, "encryption_keys": 3, "privatekeys": 4, "private_key_pem": 5, "privateKeyPEM": 6,
			"jwt": 7, "id_jwt_raw": 8, "passcode": 9, "authHeader": 10, "auth_header": 11,
			"session": "s", "sessions": "ss", "author": "a", "keystone": "k",
		}, `{"apiKeys":"[redacted]","api_keys":"[redacted]","authHeader":"[redacted]","auth_header":"[redacted]",` +
			`"author":"a","encryption_keys":"[redacted]","id_jwt_raw":"[redacted]","jwt":"[redacted]","keystone":"k",` +
			`"passcode":"[redacted]","privateKeyPEM":"[redacted]","private_key_pem":"[redacted]","privatekeys":"[redacted]",` +
			`"session":"s","sessions":"ss"}`},
		{"auth and pass suffixes", map[string]any{"basic_auth": 1, "proxyAuth": 2, "db_pass": 3, "pass": 4, "bypass": 5, "passenger": "p"},
			`{"basic_auth":"[redacted]","bypass":"[redacted]","db_pass":"[redacted]","pass":"[redacted]","passenger":"p","proxyAuth":"[redacted]"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := redactedJSON(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
	if _, err := redactedJSON(func() {}); err == nil {
		t.Fatal("want error on unmarshalable value")
	}
	if _, err := redactedJSON(cyclic()); err == nil {
		t.Fatal("want error on a cyclic value")
	}
}

// 001-AC21: a failed audit write rolls the mutation back with it.
func TestAuditFailureRollsBackMutation(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	for name, e := range map[string]Entry{
		"check constraint": {Action: "", TargetType: "user"},
		"unknown actor":    {ActorUserID: "00000000-0000-0000-0000-000000000000", Action: "user.created", TargetType: "user"},
		"unmarshalable":    {Action: "user.created", TargetType: "user", After: map[string]any{"f": func() {}}},
		"cyclic value":     {Action: "user.created", TargetType: "user", After: cyclic()},
	} {
		t.Run(name, func(t *testing.T) {
			sub := "rollback-" + name
			err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `INSERT INTO users (oidc_sub) VALUES ($1)`, sub); err != nil {
					return err
				}
				return Record(ctx, tx, e)
			})
			if err == nil {
				t.Fatal("want the audit write to fail")
			}
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE oidc_sub = $1`, sub).Scan(&n); err != nil || n != 0 {
				t.Fatalf("mutation survived a failed audit write: n=%d err=%v", n, err)
			}
		})
	}

	// And the happy path commits both, with the request ID taken from ctx.
	rctx := WithRequestID(ctx, "req-1")
	var uid string
	err := db.InTx(rctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(rctx, `INSERT INTO users (oidc_sub) VALUES ('ok') RETURNING id::text`).Scan(&uid); err != nil {
			return err
		}
		return Record(rctx, tx, Entry{ActorUserID: uid, Action: "user.created", TargetType: "user", TargetID: uid, After: map[string]string{"password": "p"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	var after, reqID string
	if err := pool.QueryRow(ctx, `SELECT after::text, request_id FROM audit_log WHERE target_id = $1`, uid).Scan(&after, &reqID); err != nil {
		t.Fatal(err)
	}
	if after != `{"password": "[redacted]"}` || reqID != "req-1" {
		t.Fatalf("stored after=%s request_id=%s", after, reqID)
	}
}

// The spec calls the log append-only; the database refuses edits and deletes.
func TestAppendOnly(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	uid := newUser(t, pool, "u")
	if err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
		return Record(ctx, tx, Entry{ActorUserID: uid, Action: "user.login", TargetType: "user"})
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`UPDATE audit_log SET action = 'x'`, `DELETE FROM audit_log`, `TRUNCATE audit_log`} {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Errorf("%s: allowed", q)
		}
	}
}

func seed(t *testing.T, pool *pgxpool.Pool, actor string, n int, targetType string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		if _, err := pool.Exec(ctx, `INSERT INTO audit_log (at, actor_user_id, action, target_type, target_id) VALUES ($1, $2, 'x.y', $3, $4)`,
			at.Add(time.Duration(i)*time.Second), actor, targetType, targetType+"-"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
}

// 001-AC22: newest first, keyset pages without gaps or repeats, filters by actor, target type and date.
func TestList(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	alice, bob := newUser(t, pool, "alice"), newUser(t, pool, "bob")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seed(t, pool, alice, 5, "user", base)
	seed(t, pool, alice, 3, "watch", base.Add(time.Hour))
	seed(t, pool, bob, 4, "user", base)
	// Same timestamp ties are broken by id.
	if _, err := pool.Exec(ctx, `INSERT INTO audit_log (at, actor_user_id, action, target_type) SELECT $1, $2, 'tie', 'user' FROM generate_series(1, 3)`, base.Add(-time.Hour), alice); err != nil {
		t.Fatal(err)
	}

	var all []Item
	cursor := ""
	for page := 0; ; page++ {
		items, next, err := List(ctx, pool, Query{Viewer: alice, Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, items...)
		if next == "" {
			break
		}
		cursor = next
		if page > 10 {
			t.Fatal("pagination does not end")
		}
	}
	if len(all) != 11 {
		t.Fatalf("want 11 entries for alice, got %d", len(all))
	}
	seen := map[int64]bool{}
	for i, it := range all {
		if seen[it.ID] {
			t.Fatalf("id %d repeated", it.ID)
		}
		seen[it.ID] = true
		// Visibility: bob's rows never leak into alice's view.
		if it.ActorUserID == nil || *it.ActorUserID != alice || it.ActorName != "alice" {
			t.Fatalf("foreign or unnamed row: %+v", it)
		}
		if i > 0 {
			prev := all[i-1]
			if it.At.After(prev.At) || (it.At.Equal(prev.At) && it.ID > prev.ID) {
				t.Fatalf("not newest first at %d", i)
			}
		}
	}

	count := func(q Query) int {
		t.Helper()
		q.Viewer, q.Limit = alice, 200
		items, _, err := List(ctx, pool, q)
		if err != nil {
			t.Fatal(err)
		}
		return len(items)
	}
	for name, tc := range map[string]struct {
		q    Query
		want int
	}{
		"target type":         {Query{TargetType: "watch"}, 3},
		"from inclusive":      {Query{From: base.Add(time.Hour)}, 3},
		"to exclusive":        {Query{To: base}, 3},
		"range":               {Query{From: base, To: base.Add(2 * time.Second)}, 2},
		"actor self":          {Query{Actor: alice}, 11},
		"actor other is none": {Query{Actor: bob}, 0},
	} {
		if got := count(tc.q); got != tc.want {
			t.Errorf("%s: want %d got %d", name, tc.want, got)
		}
	}

	for _, bad := range []string{"!!", encodeCursor(base, 1)[:3], "Zm9v", "YS5i"} {
		if _, _, err := List(ctx, pool, Query{Viewer: alice, Limit: 1, Cursor: bad}); !errors.Is(err, ErrBadCursor) {
			t.Errorf("cursor %q: want ErrBadCursor, got %v", bad, err)
		}
	}
	if _, _, err := List(ctx, pool, Query{Limit: 1}); err == nil {
		t.Error("a list without viewer must fail")
	}
}

func TestItemJSON(t *testing.T) {
	b, err := json.Marshal(Item{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["before"] != nil || m["actor_user_id"] != nil {
		t.Fatalf("absent before/actor must be null: %s", b)
	}
}

// account is a phase 3+ entity done right: the secret field never reaches its JSON form.
type account struct {
	Name        string `json:"name"`
	PlainSecret string `json:"-"`
	APIKey      string `json:"api_key,omitempty"`
}

type masked struct {
	User   string
	Hidden string
}

func (m masked) MarshalJSON() ([]byte, error) { return json.Marshal(map[string]string{"user": m.User}) }

type node struct {
	Next *node `json:"next"`
}

func cyclic() *node {
	n := &node{}
	n.Next = n
	return n
}
