package reporting

import "embed"

// templateFS embeds the per-jurisdiction confirmation templates
// (EU = MiFID II Art. 25 default; UK = FCA COBS 16; US = CFTC retail
// forex). Compliance wording is overridable per jurisdiction via
// Options.Disclosures without touching the template files.
//
//go:embed templates/*.tmpl
var templateFS embed.FS
