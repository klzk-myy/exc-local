package main

import (
	"crypto/sha256"
	"fmt"

	"github.com/spf13/cobra"

	"exchange/internal/auth"
	"exchange/internal/compliance"
	"exchange/internal/config"
)

// sealTaxPIICmd is the migration-210 data step (PII-F1): the schema
// adds tin_sealed/fields_sealed but the crypto lives in the app layer,
// so the plaintext→sealed migration runs here, not in SQL.
//
//	exchange seal-tax-pii            seal legacy plaintext rows, NULL the
//	                                 deprecated tin/fields columns
//	exchange seal-tax-pii --restore  unseal back into tin/fields — the
//	                                 required pre-step for the 210 down
//	                                 migration (privacy regression)
//
// Key source is the same envelope key as the gateway: secrets.data_key
// (EXC_SECRETS_DATA_KEY, base64/hex 32 bytes; Vault/KMS in production).
// Non-production without a key uses the same deterministic dev fallback
// as cmd/gateway — sealed data stays interoperable across both binaries.
var sealTaxPIICmd = &cobra.Command{
	Use:   "seal-tax-pii [--restore]",
	Short: "Seal (or restore) tax_self_certifications PII columns",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		restore, _ := cmd.Flags().GetBool("restore")
		ctx := cmd.Context()

		cfg, err := config.Load()
		if err != nil {
			return err
		}
		dataKey, kerr := config.DecodeDataKey(cfg.Secrets.DataKey)
		if kerr != nil {
			// Mirror of cmd/gateway's non-production fallback — keep the
			// passphrase byte-identical or sealed rows will not open.
			if cfg.IsProduction() {
				return fmt.Errorf("secrets.data_key required (EXC_SECRETS_DATA_KEY)")
			}
			dev := sha256.Sum256([]byte("exc.local non-production secret-box data key v1"))
			dataKey = dev[:]
			fmt.Fprintln(cmd.ErrOrStderr(),
				"seal-tax-pii: secrets.data_key unset — non-production deterministic dev key in use")
		}
		box, err := auth.NewSecretBox(dataKey)
		if err != nil {
			return fmt.Errorf("secret box: %w", err)
		}
		pool, err := openPool(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		store, err := compliance.NewPgStore(pool, box)
		if err != nil {
			return err
		}
		if restore {
			n, err := store.UnsealTaxPII(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("seal-tax-pii --restore: %d rows restored to plaintext "+
				"(run 210 down only after this)\n", n)
			return nil
		}
		n, err := store.SealTaxPIIBackfill(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("seal-tax-pii: %d legacy rows sealed, plaintext columns cleared\n", n)
		return nil
	},
}

func init() {
	sealTaxPIICmd.Flags().Bool("restore", false,
		"unseal tin_sealed/fields_sealed back into the plaintext columns (pre-down-migration step)")
}
