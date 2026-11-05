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
	log.Printf("fakeupstream listening on %s", *addr)
	log.Fatal(newServer(*addr).ListenAndServe())
}

func newServer(addr string) *http.Server {
	// Above the sender's 90 s idle timeout (netguard): this timer runs from accept, so it also closes
	// a connection the sender dialed but never used, and resets the POST that picks it at that moment.
	return &http.Server{Addr: addr, Handler: testupstream.New(), ReadHeaderTimeout: 2 * time.Minute}
}
