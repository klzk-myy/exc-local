package reporting

import (
	"bytes"
	"fmt"
	"strings"
)

// Minimal PDF-1.4 writer — same approach as internal/tax/render.go
// (hand-rolled, Courier, one content stream per page, ~44 lines/page).
// Kept in reporting rather than api so both the confirmation service
// and the analytics RTS-28 renderer share one implementation without
// dragging an HTTP dependency into either.

// PDFLine is one text row at the given font size.
type PDFLine struct {
	Text string
	Size int
}

// RenderPDFDoc produces a valid PDF-1.4 document from the given lines.
// Pagination at 44 lines/page; empty input still yields a valid
// single-page document.
func RenderPDFDoc(lines []PDFLine) []byte {
	pages := [][]PDFLine{}
	cur := []PDFLine{}
	flush := func() {
		if len(cur) > 0 {
			pages = append(pages, cur)
			cur = nil
		}
	}
	for _, l := range lines {
		cur = append(cur, l)
		if len(cur) >= 44 {
			flush()
		}
	}
	flush()
	if len(pages) == 0 {
		pages = [][]PDFLine{{{Text: "(empty document)", Size: 10}}}
	}

	var out bytes.Buffer
	offsets := []int{}
	obj := func(id int, body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", id, body)
	}
	out.WriteString("%PDF-1.4\n")
	n := len(pages)
	kids := ""
	for i := 0; i < n; i++ {
		kids += fmt.Sprintf("%d 0 R ", 4+i*2)
	}
	obj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	obj(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, n))
	obj(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Courier >>")
	for i, pg := range pages {
		contentID := 4 + i*2 + 1
		obj(4+i*2, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "+
				"/Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>",
			contentID))
		var stream bytes.Buffer
		stream.WriteString("BT\n40 750 Td\n")
		for _, l := range pg {
			size := l.Size
			if size < 6 {
				size = 6
			}
			fmt.Fprintf(&stream, "/F1 %d Tf (%s) Tj 0 -%d Td\n",
				size, pdfEscape(l.Text), size+6)
		}
		stream.WriteString("ET")
		obj(contentID, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream",
			stream.Len(), stream.String()))
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", 4+2*n)
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		4+2*n, xref)
	return out.Bytes()
}

func pdfEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `(`, `\(`)
	s = strings.ReplaceAll(s, `)`, `\)`)
	// PDF/Helvetica is latin-1 — strip non-latin runes rather than
	// corrupting the content stream.
	var b strings.Builder
	for _, r := range s {
		if r > 255 {
			b.WriteRune('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
