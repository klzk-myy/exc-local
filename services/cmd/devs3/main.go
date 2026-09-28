// Command devs3 runs the filesystem-backed S3-compatible dev server
// (internal/devs3). It exists for local development and CI integration
// tests of the S3 archive / WORM pipelines without docker or AWS.
//
//	devs3 --listen=127.0.0.1:4599 --root=/tmp/devs3
//
// Point clients at it with EXC_S3_ENDPOINT=http://127.0.0.1:4599 — the
// objectstore dev client forces path-style addressing and devs3 ignores
// auth, so any static credentials work.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"exchange/internal/devs3"
)

func main() {
	listen := flag.String("listen", envOr("DEVS3_LISTEN", "127.0.0.1:4599"), "listen address")
	root := flag.String("root", envOr("DEVS3_ROOT", filepath.Join(os.TempDir(), "devs3-data")), "storage root")
	flag.Parse()

	srv, err := devs3.New(*root)
	if err != nil {
		log.Fatalf("devs3: %v", err)
	}
	fmt.Printf("devs3: listening on http://%s root=%s\n", *listen, *root)
	if err := http.ListenAndServe(*listen, srv); err != nil {
		log.Fatalf("devs3: %v", err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
