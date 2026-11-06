package server_test

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Names only watch sharing used: PostgreSQL FOR SHARE / KEY SHARE and "shared secret" never match.
var shareNames = regexp.MustCompile(`(?i)watch_shares|sharee|shared_with|addShare|SharedBadge|with_me|levelWrite|shares_received|SharedWatch|dropped_share`)

// Sharing's history: the migrations that create and drop it, the test of the drop, and this guard.
var shareAllowed = map[string]bool{
	"pkg/db/migrations/00003_watches.sql":           true,
	"pkg/db/migrations/00017_drop_watch_shares.sql": true,
	"pkg/db/share_drop_test.go":                     true,
	"pkg/server/noshare_test.go":                    true,
}

// 009-AC13: no live code, test or page names watch sharing again.
func TestNoWatchSharingLeft(t *testing.T) {
	root := os.DirFS("../..")
	var hits []string
	for _, dir := range []string{"pkg", "cmd", "web/src", "web/e2e"} {
		err := fs.WalkDir(root, dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || shareAllowed[path] {
				return nil
			}
			raw, err := fs.ReadFile(root, path)
			if err != nil {
				return err
			}
			if m := shareNames.Find(raw); m != nil {
				hits = append(hits, path+": "+string(m))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(hits) > 0 {
		t.Fatalf("watch sharing names left:\n%s", strings.Join(hits, "\n"))
	}
}
