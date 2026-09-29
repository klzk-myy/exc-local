package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"exchange/internal/auth"
	"exchange/internal/config"
	"exchange/internal/security"
)

// loadJWTFromSecretSource resolves JWT signing/verification material via the
// Phase-13.5 secret-source seam (Task 13.5.3.5). Production resolves the
// Vault KV path exchange/<env>/jwt/signing (override EXC_VAULT_JWT_PATH);
// development may use the labeled dev adapters (dev-file/dev-env). Source
// construction or a missing required secret fails boot closed.
func loadJWTFromSecretSource(ctx context.Context, cfg *config.Config, iss *auth.Issuer, log *slog.Logger) error {
	src, required, err := security.SourceFromEnv(cfg.Environment)
	if err != nil {
		return fmt.Errorf("jwt secret source: %w", err)
	}
	path := os.Getenv("EXC_VAULT_JWT_PATH")
	if path == "" {
		path = "exchange/" + cfg.Environment + "/jwt/signing"
	}
	bundle, err := security.LoadSecrets(ctx, src, []security.SecretRef{{
		Name:     "jwt-signing",
		Class:    security.ClassJWTSigningKey,
		Path:     path,
		Required: true,
	}}, required || cfg.IsProduction())
	if err != nil {
		return fmt.Errorf("jwt secret load: %w", err)
	}
	fields := bundle.Secrets["jwt-signing"].Fields
	// The dev-env adapter returns a single {EXC_DEV_SECRET_JWT_SIGNING: b64}
	// field — treat a kid-less payload as a bare HS256 secret under kid "v1",
	// matching the legacy EXC_JWT_HS256_KEY_B64 bootstrap semantics.
	if _, ok := fields["kid"]; !ok && len(fields) == 1 {
		for _, v := range fields {
			fields = map[string]string{"kid": "v1", "alg": "HS256", "hs256_b64": v}
		}
	}
	kv, err := security.JWTMaterialFromFields(fields)
	if err != nil {
		return fmt.Errorf("jwt material: %w", err)
	}
	var ierr error
	switch kv.Alg {
	case "RS256":
		ierr = iss.AddRSAKey(kv.KID, kv.RSAPriv, kv.RSAPub, true)
	case "EdDSA":
		ierr = iss.AddEd25519Key(kv.KID, kv.EdPriv, kv.EdPub, true)
	default:
		ierr = iss.AddHMACKey(kv.KID, kv.HMACSecret, true)
	}
	if ierr != nil {
		return fmt.Errorf("jwt key install: %w", ierr)
	}
	log.Info("jwt verify keyring: 1 key via secret source",
		"source", src.Name(), "path", path, "kid", kv.KID, "alg", kv.Alg)
	return nil
}
