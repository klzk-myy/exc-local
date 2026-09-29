// strutil.go — shared small string helpers for the admin package.
package admin

// trunc255 bounds a ledger/audit description at 255 bytes (the journal
// description column width). Byte-level cut is fine — descriptions are
// ASCII operator prose.
func trunc255(s string) string {
	if len(s) > 255 {
		return s[:255]
	}
	return s
}
