// Command fakeupstream serves the recorded GitHub, GitLab and registry fixtures for the e2e run.
// POST /_fixture/b switches every route to the changed fixture set.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/gedour/gedour/internal/testupstream"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18090", "listen address")
	flag.Parse()
	srv := &http.Server{Addr: *addr, Handler: testupstream.New(), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("fakeupstream listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}
