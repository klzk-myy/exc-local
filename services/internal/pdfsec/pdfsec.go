// Package pdfsec adds standard-security-handler encryption to the
// repository's hand-rolled PDF-1.4 documents (internal/api/pdfdoc.go,
// internal/reporting/pdfdoc.go, internal/analytics statements).
//
// Spec §16 + Phase-20 Task 20.3.8 DoD: client-facing documents
// (confirmations, statements, invoices) must leave the platform as
// encrypted PDFs; §5.28 marks the archived file location as
// "S3 / encrypted storage". Encryption at render time satisfies both —
// the object in S3 and the email attachment are the same ciphertext.
//
// Algorithm: PDF 1.6 §3.5 standard security handler, V=4 / R=4,
// crypt filter AESV2 (AES-128-CBC, PKCS7, random per-object IV).
// O/U computation still uses RC4 per the spec (V ≤ 4); only stream and
// string contents use AES. Everything is stdlib — no external PDF dep.
package pdfsec

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
)

// passwordPadding is the fixed 32-byte pad from PDF 1.6 Table 3.19.
var passwordPadding = []byte{
	0x28, 0xBF, 0x4E, 0x5E, 0x4E, 0x75, 0x8A, 0x41,
	0x64, 0x00, 0x4E, 0x56, 0xFF, 0xFA, 0x01, 0x08,
	0x2E, 0x2E, 0x00, 0xB6, 0xD0, 0x68, 0x3E, 0x80,
	0x2F, 0x0C, 0xA9, 0xFE, 0x64, 0x53, 0x69, 0x7A,
}

// permAll is /P = -4: every permission bit set (encrypts content without
// restricting print/copy — an authenticity wrapper, not DRM).
const permAll = -4

var (
	objHeader    = regexp.MustCompile(`(\d+) (\d+) obj\n`)
	literalOpen  = byte('(')
	literalClose = byte(')')
)

func padPassword(pw string) []byte {
	b := make([]byte, 32)
	copy(b, pw)
	copy(b[len(pw):], passwordPadding)
	return b[:32]
}

func rc4(key, data []byte) []byte {
	var s [256]byte
	for i := range s {
		s[i] = byte(i)
	}
	j := 0
	for i := 0; i < 256; i++ {
		j = (j + int(s[i]) + int(key[i%len(key)])) & 0xff
		s[i], s[j] = s[j], s[i]
	}
	out := make([]byte, len(data))
	x, y := 0, 0
	for i := range data {
		x = (x + 1) & 0xff
		y = (y + int(s[x])) & 0xff
		s[x], s[y] = s[y], s[x]
		out[i] = data[i] ^ s[(int(s[x])+int(s[y]))&0xff]
	}
	return out
}

// md5Iter returns the first n bytes of 50× iterated MD5 (R≥3 hashing).
func md5Iter(seed []byte, n int) []byte {
	h := md5.Sum(seed)
	d := h[:n]
	for i := 0; i < 50; i++ {
		h = md5.Sum(d)
		d = h[:n]
	}
	return d
}

// xorKey XORs every byte of key with i (per-object RC4 iteration).
func xorKey(key []byte, i int) []byte {
	k := make([]byte, len(key))
	for j := range k {
		k[j] = key[j] ^ byte(i)
	}
	return k
}

// ownerEntry computes the /O value (R=4): RC4 chain over the padded
// owner password, then encrypts the padded user password 20 times.
func ownerEntry(userPW, ownerPW string) []byte {
	if ownerPW == "" {
		ownerPW = userPW
	}
	key := md5Iter(padPassword(ownerPW), 16)
	o := padPassword(userPW)
	for i := 0; i < 20; i++ {
		o = rc4(xorKey(key, i), o)
	}
	return o
}

// fileKey derives the 128-bit document key (Algorithm 3.2, R=4).
func fileKey(userPW string, o []byte, id0 []byte) []byte {
	p := make([]byte, 4)
	binary.LittleEndian.PutUint32(p, uint32(permAll&0xffffffff))
	h := md5.New()
	h.Write(padPassword(userPW))
	h.Write(o)
	h.Write(p)
	h.Write(id0)
	seed := h.Sum(nil)
	return md5Iter(seed[:16], 16)
}

// userEntry computes /U (Algorithm 3.5): RC4 over
// MD5(padding + ID0) with 19 XOR-iterated passes, padded to 32 bytes.
func userEntry(key, id0 []byte) []byte {
	h := md5.New()
	h.Write(passwordPadding)
	h.Write(id0)
	u := rc4(key, h.Sum(nil))
	for i := 1; i < 20; i++ {
		u = rc4(xorKey(key, i), u)
	}
	out := make([]byte, 32)
	copy(out, u)
	copy(out[16:], passwordPadding[:16])
	return out
}

// objectKey derives the per-object AES key (Algorithm 3.1 + "sAlT"
// extension for AESV2): MD5(fileKey + obj3LE + gen2LE + sAlT)[:16].
func objectKey(fk []byte, objNum, genNum int) []byte {
	var tail [5]byte
	tail[0] = byte(objNum)
	tail[1] = byte(objNum >> 8)
	tail[2] = byte(objNum >> 16)
	tail[3] = byte(genNum)
	tail[4] = byte(genNum >> 8)
	h := md5.New()
	h.Write(fk)
	h.Write(tail[:])
	h.Write([]byte("sAlT"))
	sum := h.Sum(nil)
	return sum[:16]
}

// aesEncrypt prepends a random 16-byte IV and AES-128-CBC encrypts with
// PKCS7 padding — the AESV2 wire form.
func aesEncrypt(key, plaintext []byte) ([]byte, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	pt := make([]byte, len(plaintext)+pad)
	copy(pt, plaintext)
	for i := len(plaintext); i < len(pt); i++ {
		pt[i] = byte(pad)
	}
	ct := make([]byte, len(pt))
	cipher.NewCBCEncrypter(blk, iv).CryptBlocks(ct, pt)
	return append(iv, ct...), nil
}

// streamLengthRe pulls the declared byte count out of a stream dict.
var streamLengthRe = regexp.MustCompile(`/Length (\d+)`)

// streamPayload bounds a stream's payload. The dict's /Length is the
// authoritative boundary — required for ciphertext, which is binary and
// can end in 0x0a (a delimiter-suffix trim silently eats a legitimate
// block byte: observed as "malformed ciphertext len 223", ~0.4%/stream).
// Delimiter fallback covers docs lacking /Length.
func streamPayload(dict, rest []byte) []byte {
	if m := streamLengthRe.FindSubmatch(dict); m != nil {
		if n, err := strconv.Atoi(string(m[1])); err == nil && n >= 0 && n <= len(rest) {
			return rest[:n]
		}
	}
	return bytes.TrimSuffix(rest, []byte("\nendstream"))
}

// aesDecrypt peels the IV and AES-128-CBC decrypts with PKCS7 unpad.
func aesDecrypt(key, blob []byte) ([]byte, error) {
	if len(blob) < aes.BlockSize*2 || len(blob)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("pdfsec: malformed ciphertext len %d", len(blob))
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pt := make([]byte, len(blob)-aes.BlockSize)
	cipher.NewCBCDecrypter(blk, blob[:aes.BlockSize]).CryptBlocks(pt, blob[aes.BlockSize:])
	pad := int(pt[len(pt)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(pt) {
		return nil, fmt.Errorf("pdfsec: bad PKCS7 padding")
	}
	for _, b := range pt[len(pt)-pad:] {
		if int(b) != pad {
			return nil, fmt.Errorf("pdfsec: bad PKCS7 padding")
		}
	}
	return pt[:len(pt)-pad], nil
}

// objectSpan is one parsed indirect object.
type objectSpan struct {
	num, gen  int
	bodyStart int // index of first body byte (after "N G obj\n")
	bodyEnd   int // index of "endobj"
}

// findObjects locates every "N G obj\n…endobj" span.
func findObjects(doc []byte) ([]objectSpan, error) {
	var spans []objectSpan
	matches := objHeader.FindAllSubmatchIndex(doc, -1)
	for _, m := range matches {
		num, _ := strconv.Atoi(string(doc[m[2]:m[3]]))
		gen, _ := strconv.Atoi(string(doc[m[4]:m[5]]))
		bodyStart := m[1]
		end := bytes.Index(doc[bodyStart:], []byte("\nendobj"))
		if end < 0 {
			return nil, fmt.Errorf("pdfsec: object %d %d missing endobj", num, gen)
		}
		spans = append(spans, objectSpan{num: num, gen: gen, bodyStart: bodyStart, bodyEnd: bodyStart + end})
	}
	if len(spans) == 0 {
		return nil, fmt.Errorf("pdfsec: no indirect objects found")
	}
	return spans, nil
}

// encryptLiterals AES-encrypts every (...) literal string inside a body
// slice and emits it as a hex string <…>. Escapes (\( \) \\) are
// handled; unbalanced input fails closed.
func encryptLiterals(body []byte, objNum, gen int, fk []byte) ([]byte, error) {
	var out bytes.Buffer
	i := 0
	for i < len(body) {
		c := body[i]
		if c != literalOpen {
			out.WriteByte(c)
			i++
			continue
		}
		// Find the matching unescaped close paren; strings may nest
		// parens only via escape.
		depth := 1
		j := i + 1
		for j < len(body) && depth > 0 {
			switch body[j] {
			case '\\':
				j += 2
				continue
			case literalOpen:
				depth++
			case literalClose:
				depth--
			}
			j++
		}
		if depth != 0 {
			return nil, fmt.Errorf("pdfsec: unbalanced literal string in object %d", objNum)
		}
		inner := body[i+1 : j-1]
		// Unescape before encrypting so decryption yields the original
		// text bytes.
		var plain bytes.Buffer
		for k := 0; k < len(inner); k++ {
			if inner[k] == '\\' && k+1 < len(inner) {
				k++
			}
			plain.WriteByte(inner[k])
		}
		ct, err := aesEncrypt(objectKey(fk, objNum, gen), plain.Bytes())
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&out, "<%X>", ct)
		i = j
	}
	return out.Bytes(), nil
}

// EncryptPDF encrypts every stream and literal string in a minimal
// PDF-1.4 document (the format emitted by this repo's pdfdoc writers)
// and appends the /Encrypt + /ID trailer entries. userPassword is the
// document-open password; ownerPassword empty ⇒ same as user.
//
// Fail-closed: any structural surprise (missing endobj, unbalanced
// string) aborts rather than emitting a half-encrypted document.
func EncryptPDF(doc []byte, userPassword, ownerPassword string) ([]byte, error) {
	if len(userPassword) == 0 {
		return nil, fmt.Errorf("pdfsec: user password required")
	}
	if !bytes.HasPrefix(doc, []byte("%PDF-")) {
		return nil, fmt.Errorf("pdfsec: not a PDF document")
	}
	spans, err := findObjects(doc)
	if err != nil {
		return nil, err
	}
	// Document ID: MD5 of now+random — stable within this file.
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, err
	}
	id0 := md5.Sum(append(rnd[:], doc[:64]...))
	o := ownerEntry(userPassword, ownerPassword)
	fk := fileKey(userPassword, o, id0[:])
	u := userEntry(fk, id0[:])

	var out bytes.Buffer
	out.WriteString("%PDF-1.5\n%\xE2\xE3\xCF\xD3\n") // binary marker line
	offsets := map[int]int{}
	maxObj := 0
	for _, sp := range spans {
		body := doc[sp.bodyStart:sp.bodyEnd]
		offsets[sp.num] = out.Len()
		fmt.Fprintf(&out, "%d %d obj\n", sp.num, sp.gen)
		if si := bytes.Index(body, []byte("stream\n")); si >= 0 {
			// Dict part before stream stays plaintext; stream payload
			// is encrypted whole.
			dict := body[:si]
			payload := streamPayload(dict, body[si+len("stream\n"):])
			ct, err := aesEncrypt(objectKey(fk, sp.num, sp.gen), payload)
			if err != nil {
				return nil, err
			}
			// /Length must describe the ciphertext size.
			var dbuf bytes.Buffer
			dbuf.Write(dict)
			// rewrite the /Length entry
			re := regexp.MustCompile(`/Length \d+`)
			d := re.ReplaceAll(dbuf.Bytes(), []byte(fmt.Sprintf("/Length %d", len(ct))))
			d, err = encryptLiterals(d, sp.num, sp.gen, fk)
			if err != nil {
				return nil, err
			}
			out.Write(d)
			fmt.Fprintf(&out, "stream\n%s\nendstream", string(ct))
		} else {
			enc, err := encryptLiterals(body, sp.num, sp.gen, fk)
			if err != nil {
				return nil, err
			}
			out.Write(enc)
		}
		out.WriteString("\nendobj\n")
		if sp.num > maxObj {
			maxObj = sp.num
		}
	}
	// /Encrypt object gets the next free object number.
	encNum := maxObj + 1
	offsets[encNum] = out.Len()
	fmt.Fprintf(&out, "%d 0 obj\n<< /Filter /Standard /V 4 /R 4 /Length 128 /P %d\n"+
		"/O <%X> /U <%X>\n"+
		"/CF << /StdCF << /AuthEvent /DocOpen /CFM /AESV2 /Length 128 >> >>\n"+
		"/StrF /StdCF /StmF /StdCF >>\nendobj\n",
		encNum, permAll, o, u)

	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n", encNum+1)
	fmt.Fprintf(&out, "0000000000 65535 f \n")
	for i := 1; i <= encNum; i++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R /Encrypt %d 0 R /ID [<%X><%X>] >>\n"+
		"startxref\n%d\n%%%%EOF\n", encNum+1, encNum, id0, id0, xref)
	return out.Bytes(), nil
}

// DecryptPDF reverses EncryptPDF — used by tests and ops tooling to
// verify document integrity. Streams are decrypted; literal strings are
// decrypted back to (...) literals.
func DecryptPDF(doc []byte, userPassword string) ([]byte, error) {
	spans, err := findObjects(doc)
	if err != nil {
		return nil, err
	}
	// Pull /ID[0] out of the trailer.
	mi := bytes.LastIndex(doc, []byte("/ID [<"))
	if mi < 0 {
		return nil, fmt.Errorf("pdfsec: no /ID in trailer")
	}
	rest := doc[mi+6:]
	var id0 [16]byte
	hi := bytes.Index(rest, []byte(">"))
	if hi < 0 || hi != 32 {
		return nil, fmt.Errorf("pdfsec: malformed /ID")
	}
	if _, err := hex.Decode(id0[:], rest[:32]); err != nil {
		return nil, fmt.Errorf("pdfsec: /ID parse: %w", err)
	}
	// The file key needs the stored /O value from the /Encrypt object —
	// user-password authentication derives the key from O + ID + P.
	var oHex []byte
	for _, sp := range spans {
		body := doc[sp.bodyStart:sp.bodyEnd]
		if !bytes.Contains(body, []byte("/Filter /Standard")) {
			continue
		}
		oi := bytes.Index(body, []byte("/O <"))
		if oi < 0 {
			return nil, fmt.Errorf("pdfsec: /O missing from Encrypt dict")
		}
		oseg := body[oi+4:]
		end := bytes.IndexByte(oseg, '>')
		if end < 0 || end != 64 {
			return nil, fmt.Errorf("pdfsec: malformed /O")
		}
		oHex = make([]byte, 32)
		if _, err := hex.Decode(oHex, oseg[:64]); err != nil {
			return nil, fmt.Errorf("pdfsec: /O decode: %w", err)
		}
	}
	if oHex == nil {
		return nil, fmt.Errorf("pdfsec: no /Encrypt object")
	}
	fk := fileKey(userPassword, oHex, id0[:])

	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := map[int]int{}
	maxObj := 0
	hexRe := regexp.MustCompile(`<([0-9A-Fa-f]{2})+>`)
	for _, sp := range spans {
		body := doc[sp.bodyStart:sp.bodyEnd]
		// The /Encrypt object is passed through untouched.
		if bytes.Contains(body, []byte("/Filter /Standard")) {
			continue
		}
		offsets[sp.num] = out.Len()
		fmt.Fprintf(&out, "%d %d obj\n", sp.num, sp.gen)
		if si := bytes.Index(body, []byte("stream\n")); si >= 0 {
			dict := decryptLiterals(body[:si], sp.num, sp.gen, fk)
			payload := streamPayload(body[:si], body[si+len("stream\n"):])
			pt, err := aesDecrypt(objectKey(fk, sp.num, sp.gen), payload)
			if err != nil {
				return nil, fmt.Errorf("pdfsec: object %d stream: %w", sp.num, err)
			}
			// Restore plaintext /Length.
			re := regexp.MustCompile(`/Length \d+`)
			dict = re.ReplaceAll(dict, []byte(fmt.Sprintf("/Length %d", len(pt))))
			out.Write(dict)
			fmt.Fprintf(&out, "stream\n%s\nendstream", string(pt))
		} else {
			out.Write(decryptLiterals(body, sp.num, sp.gen, fk))
		}
		out.WriteString("\nendobj\n")
		if sp.num > maxObj {
			maxObj = sp.num
		}
	}
	_ = hexRe
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", maxObj+1)
	for i := 1; i <= maxObj; i++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		maxObj+1, xref)
	return out.Bytes(), nil
}

// decryptLiterals rewrites every <HEX> hex string into a decrypted
// (...) literal. PDF treats <…> and (…) as equivalent string tokens.
func decryptLiterals(body []byte, objNum, gen int, fk []byte) []byte {
	var out bytes.Buffer
	i := 0
	for i < len(body) {
		if body[i] != '<' || (i+1 < len(body) && body[i+1] == '<') {
			// "<<" dict-open passes through.
			out.WriteByte(body[i])
			i++
			continue
		}
		end := bytes.IndexByte(body[i:], '>')
		if end < 0 {
			out.WriteByte(body[i])
			i++
			continue
		}
		hexstr := body[i+1 : i+end]
		// Only treat evenly-sized pure-hex content ≥32 bytes as a
		// ciphertext candidate; shorter hex (e.g. /ID fragments inside
		// dicts we shouldn't see here) passes through.
		var raw []byte
		if len(hexstr) >= 64 && len(hexstr)%2 == 0 {
			raw = make([]byte, len(hexstr)/2)
			ok := true
			for k := 0; k < len(raw); k++ {
				var v int
				if _, err := fmt.Sscanf(string(hexstr[2*k:2*k+2]), "%02x", &v); err != nil {
					ok = false
					break
				}
				raw[k] = byte(v)
			}
			if !ok {
				raw = nil
			}
		}
		if raw == nil {
			out.Write(body[i : i+end+1])
			i += end + 1
			continue
		}
		pt, err := aesDecrypt(objectKey(fk, objNum, gen), raw)
		if err != nil {
			// Not an encrypted string — pass through as hex.
			out.Write(body[i : i+end+1])
			i += end + 1
			continue
		}
		out.WriteByte('(')
		for _, b := range pt {
			switch b {
			case '(', ')', '\\':
				out.WriteByte('\\')
			}
			out.WriteByte(b)
		}
		out.WriteByte(')')
		i += end + 1
	}
	return out.Bytes()
}
