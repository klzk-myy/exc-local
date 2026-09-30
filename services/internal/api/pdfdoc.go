// Shared minimal PDF-1.4 document writer for API export handlers —
// extracted from the internal/tax/render.go precedent (Task 5.3.19) so
// report surfaces (Task 20.3.4 P&L, Task 20.3.6 statements) don't each
// re-roll object assembly. No external PDF dependency: single built-in
// Courier face, one content stream per page, fixed row layout, pagination
// at 44 lines/page.
//
// The writer intentionally exposes only title + text lines — callers
// format their own tabular columns with fmt.Sprintf("%-12s %12s", ...).
// Anything needing fonts/graphics/tables-of-contents belongs in a real
// renderer, not here.
package api

import (
	"bytes"
	"fmt"
	"strings"
)

// pdfdocLinesPerPage bounds one page's line count (40pt top margin,
// ~15pt leading at 9pt — matches the tax render's 44-row envelope).
const pdfdocLinesPerPage = 44

// PDFLine is one text row. Size is the font size in points; 0 selects
// the 9pt body default.
type PDFLine struct {
	Text string
	Size int
}

// RenderPDFDoc renders title (16pt) + lines into a valid PDF-1.4 file and
// returns the complete document bytes. lines are paginated at
// pdfdocLinesPerPage; an empty line slice still yields a one-page
// document carrying the title.
func RenderPDFDoc(title string, lines []PDFLine) []byte {
	pages := [][]PDFLine{}
	cur := []PDFLine{{Text: title, Size: 16}}
	flush := func() {
		if len(cur) > 0 {
			pages = append(pages, cur)
			cur = nil
		}
	}
	emit := func(l PDFLine) {
		if l.Size <= 0 {
			l.Size = 9
		}
		cur = append(cur, l)
		if len(cur) >= pdfdocLinesPerPage {
			flush()
		}
	}
	for _, l := range lines {
		emit(l)
	}
	flush()

	// Object layout: 1=catalog, 2=pages, 3=font, then per page
	// (pageObj=4+i*2, contentObj=4+i*2+1).
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
			fmt.Fprintf(&stream, "/F1 %d Tf (%s) Tj 0 -%d Td\n",
				l.Size, pdfdocEsc(l.Text), l.Size+6)
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

// pdfdocEsc escapes the three characters that are syntax inside a PDF
// literal string.
func pdfdocEsc(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `(`, `\(`)
	s = strings.ReplaceAll(s, `)`, `\)`)
	return s
}
