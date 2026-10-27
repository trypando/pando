package s3_test

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is enough of the S3 API, path-style, for the adapter: objects,
// multipart uploads and ListObjectsV2. It refuses a request not signed with
// the key ID it was made for, and can be told to fail one operation.
type fakeS3 struct {
	t      *testing.T
	bucket string
	keyID  string

	mu       sync.Mutex
	objects  map[string][]byte
	modified map[string]time.Time
	uploads  map[string]map[int][]byte
	nextID   int
	aborted  int
	pageSize int

	// failOn makes the named operation answer with failCode.
	failOn, failCode string
	// largest is the largest request body seen, to show nothing sends a
	// whole bundle at once.
	largest int
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	t.Helper()
	f := &fakeS3{
		t: t, bucket: "pando-backups", keyID: "AKIAEXAMPLE",
		objects: map[string][]byte{}, modified: map[string]time.Time{},
		uploads: map[string]map[int][]byte{}, pageSize: 1000,
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

type s3Error struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

func (f *fakeS3) fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(s3Error{Code: code, Message: code})
}

// operation names a request the way failOn does.
func operation(r *http.Request) string {
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		return "CreateMultipartUpload"
	case r.Method == http.MethodPost && q.Has("uploadId"):
		return "CompleteMultipartUpload"
	case r.Method == http.MethodPut && q.Has("partNumber"):
		return "UploadPart"
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		return "AbortMultipartUpload"
	case r.Method == http.MethodPut:
		return "PutObject"
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		return "ListObjectsV2"
	case r.Method == http.MethodGet:
		return "GetObject"
	case r.Method == http.MethodDelete:
		return "DeleteObject"
	}
	return r.Method
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.Header.Get("Authorization"), "Credential="+f.keyID+"/") {
		f.fail(w, http.StatusForbidden, "InvalidAccessKeyId")
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != f.bucket {
		f.fail(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.fail(w, http.StatusBadRequest, "IncompleteBody")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.largest = max(f.largest, len(body))
	op := operation(r)
	if op == f.failOn {
		status := http.StatusForbidden
		if f.failCode != "AccessDenied" {
			status = http.StatusBadRequest
		}
		f.fail(w, status, f.failCode)
		return
	}
	f.serve(w, r, op, key, body)
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request, op, key string, body []byte) {
	q := r.URL.Query()
	switch op {
	case "PutObject":
		f.objects[key], f.modified[key] = body, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	case "GetObject":
		obj, ok := f.objects[key]
		if !ok {
			f.fail(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		_, _ = w.Write(obj)
	case "DeleteObject":
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	case "CreateMultipartUpload":
		f.nextID++
		id := fmt.Sprintf("upload-%d", f.nextID)
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, f.bucket, key, id)
	case "UploadPart":
		n, _ := strconv.Atoi(q.Get("partNumber"))
		f.uploads[q.Get("uploadId")][n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, n))
	case "CompleteMultipartUpload":
		f.complete(w, q.Get("uploadId"), key)
	case "AbortMultipartUpload":
		delete(f.uploads, q.Get("uploadId"))
		f.aborted++
		w.WriteHeader(http.StatusNoContent)
	case "ListObjectsV2":
		f.list(w, q.Get("prefix"), q.Get("continuation-token"))
	default:
		f.fail(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func (f *fakeS3) complete(w http.ResponseWriter, id, key string) {
	parts, ok := f.uploads[id]
	if !ok {
		f.fail(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	numbers := make([]int, 0, len(parts))
	for n := range parts {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	var whole []byte
	for _, n := range numbers {
		whole = append(whole, parts[n]...)
	}
	f.objects[key], f.modified[key] = whole, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	delete(f.uploads, id)
	fmt.Fprintf(w, `<CompleteMultipartUploadResult><Key>%s</Key><ETag>"whole"</ETag></CompleteMultipartUploadResult>`, key)
}

func (f *fakeS3) list(w http.ResponseWriter, prefix, after string) {
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	truncated := len(keys) > f.pageSize
	if truncated {
		keys = keys[:f.pageSize]
	}
	var b strings.Builder
	b.WriteString(`<ListBucketResult>`)
	for _, k := range keys {
		fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>%d</Size><LastModified>%s</LastModified></Contents>`,
			k, len(f.objects[k]), f.modified[k].Format(time.RFC3339))
	}
	fmt.Fprintf(&b, `<IsTruncated>%t</IsTruncated>`, truncated)
	if truncated {
		fmt.Fprintf(&b, `<NextContinuationToken>%s</NextContinuationToken>`, keys[len(keys)-1])
	}
	b.WriteString(`</ListBucketResult>`)
	_, _ = io.WriteString(w, b.String())
}

func (f *fakeS3) object(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[key]
	return b, ok
}

func (f *fakeS3) pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

func (f *fakeS3) set(op, code string) {
	f.mu.Lock()
	f.failOn, f.failCode = op, code
	f.mu.Unlock()
}
