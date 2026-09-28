// Command openapi-gen regenerates the checked-in OpenAPI 3.1 artifact
// (Task 5.3.8) — docs/openapi/openapi.json — from the route registry +
// error-code registry. The document is DERIVED, never hand-maintained:
// the same api.OpenAPIDocument path serves it live at
// GET /api/v1/openapi.json (via gateway.OpenAPIDocFn).
//
// Usage (from the repository root or services/):
//
//	go run ./services/cmd/openapi-gen -out docs/openapi/openapi.json
//	cd services && go run ./cmd/openapi-gen -out ../docs/openapi/openapi.json
//
// -check verifies the artifact is up to date (CI mode: nonzero exit +
// diff summary when it has drifted from the registry).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"exchange/internal/api"
	"exchange/internal/errs"
	"exchange/internal/gateway"
)

// version mirrors cmd/gateway's apiVersion default; override with
// -ldflags "-X main.version=..." to keep artifact and server in lockstep.
var version = "1.0.0"

func main() {
	out := flag.String("out", "docs/openapi/openapi.json",
		"artifact path to write")
	check := flag.Bool("check", false,
		"verify the artifact matches the registry; do not write")
	flag.Parse()

	// The seed table is the normative registry input — identical to what
	// the gateway mounts (MountSeedLive registers exactly SeedRoutes()).
	doc := api.OpenAPIDocument(gateway.SeedRoutes(), errs.Default, version)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		fmt.Fprintf(os.Stderr, "openapi-gen: encode: %v\n", err)
		os.Exit(1)
	}
	generated := buf.Bytes()

	if *check {
		existing, err := os.ReadFile(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "openapi-gen: read %s: %v\n", *out, err)
			os.Exit(1)
		}
		if !bytes.Equal(existing, generated) {
			paths, ops := api.DocCount(doc)
			fmt.Fprintf(os.Stderr,
				"openapi-gen: %s is stale (registry now yields %d paths / %d operations) — regenerate with `go run ./cmd/openapi-gen -out %s`\n",
				*out, paths, ops, *out)
			os.Exit(1)
		}
		paths, ops := api.DocCount(doc)
		fmt.Printf("openapi-gen: %s is current (%d paths, %d operations)\n",
			*out, paths, ops)
		return
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "openapi-gen: mkdir: %v\n", err)
		os.Exit(1)
	}
	tmp := *out + ".tmp"
	if err := os.WriteFile(tmp, generated, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "openapi-gen: write: %v\n", err)
		os.Exit(1)
	}
	if err := os.Rename(tmp, *out); err != nil {
		fmt.Fprintf(os.Stderr, "openapi-gen: rename: %v\n", err)
		os.Exit(1)
	}
	paths, ops := api.DocCount(doc)
	fmt.Printf("openapi-gen: wrote %s (%d paths, %d operations, %d bytes)\n",
		*out, paths, ops, len(generated))
}
