// Package devs3 is a minimal filesystem-backed S3-compatible server for
// local development and integration tests (no docker, no AWS). It is the
// deterministic counterpart to internal/objectstore: real ETag=md5
// semantics, path-style addressing, lexicographic ListObjectsV2 with
// continuation tokens, user metadata, and a modelled Object Lock flag so
// the Task 4.3.7 WORM pipeline can be exercised end-to-end.
//
// NOT for production: no authentication, no multipart, no versioning.
// Buckets are directories under -root; objects are files; per-object
// metadata lives in .devs3meta/<bucket>/<escaped-key>.json sidecars.
package devs3

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const metaRoot = ".devs3meta"

// objectMeta is the sidecar JSON for one stored object.
type objectMeta struct {
	Key                   string            `json:"key"`
	ETag                  string            `json:"etag"` // hex md5, unquoted
	Size                  int64             `json:"size"`
	ContentType           string            `json:"content_type,omitempty"`
	LastModified          time.Time         `json:"last_modified"`
	Metadata              map[string]string `json:"metadata,omitempty"`
	ObjectLockMode        string            `json:"object_lock_mode,omitempty"`
	ObjectLockRetainUntil time.Time         `json:"object_lock_retain_until,omitempty"`
}

// Server is an http.Handler exposing the stub S3 API.
type Server struct {
	root string
}

// New creates a server storing everything under root (created if absent).
func New(root string) (*Server, error) {
	if root == "" {
		return nil, errors.New("devs3: root dir required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Server{root: root}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket, key := splitPath(r.URL.Path)
	if bucket == "" {
		s.xmlError(w, http.StatusBadRequest, "InvalidRequest", "bucket required")
		return
	}
	q := r.URL.Query()

	switch r.Method {
	case http.MethodPut:
		if key == "" {
			s.createBucket(w, bucket)
			return
		}
		s.putObject(w, r, bucket, key)
	case http.MethodGet, http.MethodHead:
		if key == "" {
			if _, ok := q["location"]; ok {
				s.bucketLocation(w)
				return
			}
			if q.Get("list-type") == "2" {
				if r.Method == http.MethodHead {
					s.xmlError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "HEAD on list")
					return
				}
				s.listV2(w, r, bucket, q)
				return
			}
			if r.Method == http.MethodHead {
				s.headBucket(w, bucket)
				return
			}
			s.xmlError(w, http.StatusBadRequest, "InvalidRequest", "unsupported bucket query")
			return
		}
		s.getObject(w, r, bucket, key, r.Method == http.MethodHead)
	case http.MethodDelete:
		if key == "" {
			s.xmlError(w, http.StatusNotImplemented, "NotImplemented", "delete bucket unsupported")
			return
		}
		s.deleteObject(w, bucket, key)
	default:
		s.xmlError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

func splitPath(p string) (bucket, key string) {
	p = strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}

func (s *Server) bucketDir(b string) string { return filepath.Join(s.root, b) }

func (s *Server) objPath(b, k string) string {
	return filepath.Join(s.root, b, filepath.FromSlash(k))
}

func (s *Server) metaPath(b, k string) string {
	return filepath.Join(s.root, metaRoot, b, url.PathEscape(k)+".json")
}

// --- bucket ops --------------------------------------------------------

func (s *Server) createBucket(w http.ResponseWriter, bucket string) {
	if err := os.MkdirAll(s.bucketDir(bucket), 0o755); err != nil {
		s.xmlError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.Header().Set("Location", "/"+bucket)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) headBucket(w http.ResponseWriter, bucket string) {
	if st, err := os.Stat(s.bucketDir(bucket)); err != nil || !st.IsDir() {
		s.xmlError(w, http.StatusNotFound, "NoSuchBucket", bucket)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) bucketLocation(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+
		`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
}

// --- object ops --------------------------------------------------------

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if strings.Contains(key, "..") {
		s.xmlError(w, http.StatusBadRequest, "InvalidRequest", "key may not contain '..'")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.xmlError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	sum := md5.Sum(body)
	m := objectMeta{
		Key:          key,
		ETag:         hex.EncodeToString(sum[:]),
		Size:         int64(len(body)),
		ContentType:  r.Header.Get("Content-Type"),
		LastModified: time.Now().UTC(),
		Metadata:     map[string]string{},
	}
	for name, vals := range r.Header {
		lname := strings.ToLower(name)
		if strings.HasPrefix(lname, "x-amz-meta-") && len(vals) > 0 {
			m.Metadata[strings.TrimPrefix(lname, "x-amz-meta-")] = vals[0]
		}
	}
	if v := r.Header.Get("x-amz-object-lock-mode"); v != "" {
		m.ObjectLockMode = v
	}
	if v := r.Header.Get("x-amz-object-lock-retain-until-date"); v != "" {
		if t, terr := time.Parse(time.RFC3339, v); terr == nil {
			m.ObjectLockRetainUntil = t.UTC()
		}
	}

	path := s.objPath(bucket, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.xmlError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	tmp := path + ".tmp-" + fmt.Sprint(time.Now().UnixNano())
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		s.xmlError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		s.xmlError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	mp := s.metaPath(bucket, key)
	if err := os.MkdirAll(filepath.Dir(mp), 0o755); err == nil {
		if raw, jerr := json.Marshal(m); jerr == nil {
			_ = os.WriteFile(mp, raw, 0o644)
		}
	}
	w.Header().Set("ETag", `"`+m.ETag+`"`)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) readMeta(bucket, key string) (objectMeta, bool) {
	var m objectMeta
	raw, err := os.ReadFile(s.metaPath(bucket, key))
	if err != nil {
		// Backfill: object written without sidecar — stat the file.
		st, serr := os.Stat(s.objPath(bucket, key))
		if serr != nil {
			return m, false
		}
		sum, serr := fileMD5(s.objPath(bucket, key))
		if serr != nil {
			return m, false
		}
		return objectMeta{Key: key, ETag: sum, Size: st.Size(), LastModified: st.ModTime().UTC()}, true
	}
	if json.Unmarshal(raw, &m) != nil {
		return m, false
	}
	return m, true
}

func fileMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, bucket, key string, headOnly bool) {
	m, ok := s.readMeta(bucket, key)
	if !ok {
		s.xmlError(w, http.StatusNotFound, "NoSuchKey", key)
		return
	}
	data, err := os.ReadFile(s.objPath(bucket, key))
	if err != nil {
		s.xmlError(w, http.StatusNotFound, "NoSuchKey", key)
		return
	}
	w.Header().Set("ETag", `"`+m.ETag+`"`)
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.Header().Set("Last-Modified", m.LastModified.UTC().Format(http.TimeFormat))
	if m.ContentType != "" {
		w.Header().Set("Content-Type", m.ContentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	for k, v := range m.Metadata {
		w.Header().Set("x-amz-meta-"+k, v)
	}
	if m.ObjectLockMode != "" {
		w.Header().Set("x-amz-object-lock-mode", m.ObjectLockMode)
	}
	if !m.ObjectLockRetainUntil.IsZero() {
		w.Header().Set("x-amz-object-lock-retain-until-date",
			m.ObjectLockRetainUntil.UTC().Format(time.RFC3339))
	}
	w.WriteHeader(http.StatusOK)
	if !headOnly {
		_, _ = w.Write(data)
	}
}

func (s *Server) deleteObject(w http.ResponseWriter, bucket, key string) {
	m, ok := s.readMeta(bucket, key)
	if !ok {
		// S3 delete is idempotent: missing key still returns 204.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Modelled WORM: COMPLIANCE objects inside their retention window are
	// undeletable — AccessDenied, matching real Object Lock behaviour.
	if strings.EqualFold(m.ObjectLockMode, "COMPLIANCE") &&
		m.ObjectLockRetainUntil.After(time.Now()) {
		s.xmlError(w, http.StatusForbidden, "AccessDenied",
			"Object is under Object Lock COMPLIANCE retention until "+
				m.ObjectLockRetainUntil.Format(time.RFC3339))
		return
	}
	_ = os.Remove(s.objPath(bucket, key))
	_ = os.Remove(s.metaPath(bucket, key))
	w.WriteHeader(http.StatusNoContent)
}

// --- ListObjectsV2 -----------------------------------------------------

type listContents struct {
	XMLName      xml.Name `xml:"Contents"`
	Key          string   `xml:"Key"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
	Size         int64    `xml:"Size"`
	StorageClass string   `xml:"StorageClass"`
}

type commonPrefix struct {
	XMLName xml.Name `xml:"CommonPrefixes"`
	Prefix  string   `xml:"Prefix"`
}

type listResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	Xmlns                 string         `xml:"xmlns,attr"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	MaxKeys               int32          `xml:"MaxKeys"`
	KeyCount              int32          `xml:"KeyCount"`
	IsTruncated           bool           `xml:"IsTruncated"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	EncodingType          string         `xml:"EncodingType,omitempty"`
	Contents              []listContents `xml:""`
	Prefixes              []commonPrefix `xml:""`
}

func (s *Server) listV2(w http.ResponseWriter, r *http.Request, bucket string, q url.Values) {
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	encURL := q.Get("encoding-type") == "url"
	maxKeys := int32(1000)
	if v := q.Get("max-keys"); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &maxKeys); err != nil || n != 1 || maxKeys <= 0 {
			maxKeys = 1000
		}
	}
	startAfter := ""
	if tok := q.Get("continuation-token"); tok != "" {
		if raw, err := base64.StdEncoding.DecodeString(tok); err == nil {
			startAfter = string(raw)
		}
	}

	// Collect keys in lexicographic byte order (S3 ordering).
	var keys []string
	base := s.bucketDir(bucket)
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		s.xmlError(w, http.StatusNotFound, "NoSuchBucket", bucket)
		return
	}
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == metaRoot {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".tmp") || strings.Contains(d.Name(), ".tmp-") {
			return nil
		}
		rel, rerr := filepath.Rel(base, p)
		if rerr != nil {
			return nil
		}
		keys = append(keys, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(keys)

	res := listResult{
		Xmlns:     "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:      bucket,
		Prefix:    prefix,
		MaxKeys:   maxKeys,
		Delimiter: delimiter,
	}
	if encURL {
		res.EncodingType = "url"
	}
	seenPrefixes := map[string]bool{}
	count := int32(0)
	lastKey := ""
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) || (startAfter != "" && k <= startAfter) {
			continue
		}
		if delimiter != "" {
			if i := strings.Index(k[len(prefix):], delimiter); i >= 0 {
				cp := k[:len(prefix)+i+len(delimiter)]
				if !seenPrefixes[cp] {
					seenPrefixes[cp] = true
					res.Prefixes = append(res.Prefixes, commonPrefix{Prefix: cp})
					count++
				}
				continue
			}
		}
		if count >= maxKeys {
			res.IsTruncated = true
			res.NextContinuationToken = base64.StdEncoding.EncodeToString([]byte(lastKey))
			break
		}
		m, ok := s.readMeta(bucket, k)
		size := int64(0)
		etag := `""`
		lm := time.Now().UTC()
		if ok {
			size, lm = m.Size, m.LastModified
			etag = `"` + m.ETag + `"`
		}
		outKey := k
		if encURL {
			outKey = url.QueryEscape(k)
		}
		res.Contents = append(res.Contents, listContents{
			Key:          outKey,
			LastModified: lm.Format("2006-01-02T15:04:05.000Z"),
			ETag:         etag,
			Size:         size,
			StorageClass: "STANDARD",
		})
		lastKey = k
		count++
	}
	res.KeyCount = int32(len(res.Contents)) + int32(len(res.Prefixes))

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(res)
}

// --- errors ------------------------------------------------------------

type s3Error struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

func (s *Server) xmlError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(s3Error{Code: code, Message: msg})
}
