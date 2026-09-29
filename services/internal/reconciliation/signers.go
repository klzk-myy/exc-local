// Phase-13 Task 13.3.7 — the root-signing seam.
//
// Production signature is a detached GPG signature by the exchange's
// cold-storage key: the private key never lives on this host — the runner
// shells out to `gpg --local-user <fpr>` which resolves the key from the
// operator's offline-imported keyring (HSM/agent-backed in production
// per Phase-13.5 secrets management). Selection is env-driven:
//
//	EXC_SOLVENCY_SIGNER           "gpg" | "dev-hmac" (default: gpg in
//	                              production, dev-hmac elsewhere)
//	EXC_SOLVENCY_GPG_FINGERPRINT  long key id / fingerprint of the
//	                              cold-storage signing key (required for gpg)
//	EXC_SOLVENCY_GPG_BIN          gpg binary path (default "gpg")
//	EXC_SOLVENCY_DEV_HMAC_KEY     dev-hmac key bytes (default: a fixed
//	                              NON-SECRET dev key — forgeable by anyone)
//
// Fail-closed rule: a signer that cannot sign aborts Generate — no
// unsigned root is ever published. DevHMACSigner output is prefixed
// "DEV-HMAC-SHA256:" and kind=DEV-HMAC so a dev signature can never be
// mistaken for a GPG attestation on the public endpoint.
package reconciliation

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Signer kinds persisted on solvency_snapshots.signer_kind (CHECK bound).
const (
	SignerKindGPG     = "GPG"
	SignerKindDevHMAC = "DEV-HMAC"
)

// GPGSigner signs via the local gpg binary against a key identified by
// fingerprint — the production cold-storage seam.
type GPGSigner struct {
	Fingerprint string // EXC_SOLVENCY_GPG_FINGERPRINT
	Bin         string // default "gpg"
}

// Sign runs `gpg --batch --local-user <fpr> --detach-sign --armor` over
// the payload and returns the armored signature block.
func (g GPGSigner) Sign(ctx context.Context, payload []byte) (string, string, string, error) {
	fpr := strings.TrimSpace(g.Fingerprint)
	if fpr == "" {
		return "", "", "", fmt.Errorf("reconciliation: GPG signer requires a key fingerprint")
	}
	bin := g.Bin
	if bin == "" {
		bin = "gpg"
	}
	cmd := exec.CommandContext(ctx, bin,
		"--batch", "--yes", "--trust-model", "always",
		"--local-user", fpr, "--detach-sign", "--armor", "--output", "-")
	cmd.Stdin = bytes.NewReader(payload)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", "", "", fmt.Errorf("reconciliation: gpg sign: %w: %s",
			err, strings.TrimSpace(errb.String()))
	}
	sig := out.String()
	if !strings.Contains(sig, "BEGIN PGP SIGNATURE") {
		return "", "", "", fmt.Errorf("reconciliation: gpg produced no detached signature block")
	}
	return sig, fpr, SignerKindGPG, nil
}

// DevHMACSigner is the NON-PRODUCTION stand-in: an HMAC-SHA256 over the
// payload keyed by a configured (or deterministic, clearly non-secret)
// key. It exists so dev/staging runs produce a verifiable deterministic
// artifact; it provides NO cold-storage security properties.
type DevHMACSigner struct {
	Key []byte
}

// Sign produces "DEV-HMAC-SHA256:<hex>" — deliberately unforgeable-looking
// prefix absent, deliberately labeled.
func (d DevHMACSigner) Sign(_ context.Context, payload []byte) (string, string, string, error) {
	if len(d.Key) == 0 {
		return "", "", "", fmt.Errorf("reconciliation: dev signer requires a key")
	}
	mac := hmac.New(sha256.New, d.Key)
	mac.Write(payload)
	return "DEV-HMAC-SHA256:" + hex.EncodeToString(mac.Sum(nil)),
		"DEV-HMAC", SignerKindDevHMAC, nil
}

// devHMACKey is the fixed non-secret fallback — labeled so no one can
// mistake it for production signing material.
var devHMACKey = []byte("exc.local DEV-ONLY solvency signing key — not a secret")

// SignerFromEnv resolves the signing seam. Production MUST run GPG —
// dev-hmac in production refuses construction (fail closed).
func SignerFromEnv(getenv func(string) string, environment string) (Signer, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	kind := strings.ToLower(strings.TrimSpace(getenv("EXC_SOLVENCY_SIGNER")))
	if kind == "" {
		if environment == "production" {
			kind = "gpg"
		} else {
			kind = "dev-hmac"
		}
	}
	switch kind {
	case "gpg":
		fpr := getenv("EXC_SOLVENCY_GPG_FINGERPRINT")
		if fpr == "" {
			return nil, fmt.Errorf("reconciliation: GPG signer selected but EXC_SOLVENCY_GPG_FINGERPRINT unset")
		}
		bin := getenv("EXC_SOLVENCY_GPG_BIN")
		return GPGSigner{Fingerprint: fpr, Bin: bin}, nil
	case "dev-hmac":
		if environment == "production" {
			return nil, fmt.Errorf("reconciliation: dev-hmac signer is forbidden in production")
		}
		key := []byte(getenv("EXC_SOLVENCY_DEV_HMAC_KEY"))
		if len(key) == 0 {
			key = devHMACKey
		}
		return DevHMACSigner{Key: key}, nil
	default:
		return nil, fmt.Errorf("reconciliation: unknown EXC_SOLVENCY_SIGNER %q", kind)
	}
}
