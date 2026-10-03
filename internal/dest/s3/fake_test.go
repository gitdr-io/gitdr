package s3_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	s3backend "gitdr.io/gitdr/internal/dest/s3"
)

// fakeStore is an S3 endpoint over TLS for the destination's tests. It keeps objects in memory,
// refuses what MinIO refuses (an aws-chunked chunk over 16 MiB), checks every checksum it is
// given, header or trailer, against the bytes that arrived, and takes faults.
type fakeStore struct {
	mu sync.Mutex
	// conditional answers If-None-Match: * the way AWS does. Without it the header is ignored and
	// a write to an existing key adds a version, as on any versioned bucket.
	conditional bool
	objects     map[string]*fakeObject
	accepted    []received     // every write that landed, in order
	ops         map[string]int // requests by operation
	// dropAfterCommit is how many of the next writes land and then lose their answer: the
	// connection closes with no response, so the client cannot know.
	dropAfterCommit int

	uploads    map[string]*fakeUpload
	nextUpload int
	parts      []received     // every part that landed
	completes  []completeCall // every Complete that was answered with success
	// refuseFullObject answers a full-object checksum at Create the way a store that does not
	// know the header does.
	refuseFullObject bool
	// failPart answers 500 to a part number, that many times.
	failPart map[int]int
	// completeStatus answers the next Complete with this status, and commits nothing.
	completeStatus int
	// dropCompleteAfterCommit commits the next Complete and closes the connection unanswered.
	dropCompleteAfterCommit bool
	// beforeComplete runs once, as the next Complete arrives: another writer, finishing first.
	beforeComplete func()
	// beforeHead runs before the HeadObject request of that number, counted from 1, is answered.
	beforeHead map[int]func()
}

type fakeObject struct {
	data     []byte
	crc32    string // what HeadObject returns with checksum mode on
	versions int
}

type fakeUpload struct {
	key, checksumType, algorithm, lockMode, retainUntil string
	parts                                               map[int]fakePart
	done                                                bool // completed: no more parts, no second Complete
}

type fakePart struct {
	data      []byte
	crc, etag string
}

type completeCall struct {
	ifNoneMatch, checksumType, crc32, mpuObjectSize string
}

type received struct {
	key         string
	encoding    string // Content-Encoding
	length      int64  // Content-Length
	payloadHash string // X-Amz-Content-Sha256
	crc32Header string // X-Amz-Checksum-Crc32
	lockMode    string
	retainUntil string
	data        []byte // the object, decoded from aws-chunked when it came that way
}

const minioMaxChunk = 16 << 20

func newFakeStore(t *testing.T) (*fakeStore, *httptest.Server) {
	t.Helper()
	f := &fakeStore{objects: map[string]*fakeObject{}, ops: map[string]int{}, uploads: map[string]*fakeUpload{}, failPart: map[int]int{}}
	srv := httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeStore) writes() []received {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]received(nil), f.accepted...)
}

func (f *fakeStore) partWrites() []received {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]received(nil), f.parts...)
}

func (f *fakeStore) completed() []completeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]completeCall(nil), f.completes...)
}

// lastUpload is the upload the latest Create opened, completed or not.
func (f *fakeStore) lastUpload() *fakeUpload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploads[fmt.Sprintf("upload-%d", f.nextUpload)]
}

func (f *fakeStore) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ops[op]
}

func (f *fakeStore) object(key string) *fakeObject {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[key]
}

// seed puts an object at key, as somebody else's write would have.
func (f *fakeStore) seed(key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = &fakeObject{data: data, crc32: crc32Base64(data), versions: 1}
}

func (f *fakeStore) serve(w http.ResponseWriter, r *http.Request) {
	// Path-style: /bucket/key.
	_, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodHead:
		f.head(w, r, key)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		f.uploadPart(w, r, key)
	case r.Method == http.MethodPut:
		f.putObject(w, r, key)
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.createUpload(w, r, key)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		f.completeUpload(w, r, key)
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		// Counted so a test can say it never happens.
		f.op("AbortMultipartUpload")
		w.WriteHeader(http.StatusNoContent)
	default:
		f.op("unsupported " + r.Method)
		http.Error(w, "not in this fake", http.StatusNotImplemented)
	}
}

func (f *fakeStore) createUpload(w http.ResponseWriter, r *http.Request, key string) {
	f.op("CreateMultipartUpload")
	checksumType := r.Header.Get("X-Amz-Checksum-Type")
	if checksumType != "" && f.refuseFullObject {
		s3Error(w, http.StatusBadRequest, "InvalidArgument", "Unsupported header 'x-amz-checksum-type' received for this API call.")
		return
	}
	f.mu.Lock()
	f.nextUpload++
	id := fmt.Sprintf("upload-%d", f.nextUpload)
	f.uploads[id] = &fakeUpload{
		key: key, checksumType: checksumType, algorithm: r.Header.Get("X-Amz-Checksum-Algorithm"),
		lockMode: r.Header.Get("X-Amz-Object-Lock-Mode"), retainUntil: r.Header.Get("X-Amz-Object-Lock-Retain-Until-Date"),
		parts: map[int]fakePart{},
	}
	f.mu.Unlock()
	writeXML(w, struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		UploadID string   `xml:"UploadId"`
	}{Bucket: "backups", Key: key, UploadID: id})
}

func (f *fakeStore) uploadPart(w http.ResponseWriter, r *http.Request, key string) {
	f.op("UploadPart")
	id := r.URL.Query().Get("uploadId")
	n, _ := strconv.Atoi(r.URL.Query().Get("partNumber"))
	f.mu.Lock()
	up := f.uploads[id]
	fail := f.failPart[n] > 0
	if fail {
		f.failPart[n]--
	}
	f.mu.Unlock()
	if up == nil || up.done {
		s3Error(w, http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist")
		return
	}
	if fail {
		_, _ = io.Copy(io.Discard, r.Body)
		s3Error(w, http.StatusInternalServerError, "InternalError", "injected")
		return
	}
	got, ok := readWrite(w, r, key)
	if !ok {
		return
	}
	crc := crc32Base64(got.data)
	etag := fmt.Sprintf(`"part-%d-%s"`, n, crc)
	f.mu.Lock()
	up.parts[n] = fakePart{data: got.data, crc: crc, etag: etag}
	f.parts = append(f.parts, got)
	f.mu.Unlock()
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeStore) completeUpload(w http.ResponseWriter, r *http.Request, key string) {
	f.op("CompleteMultipartUpload")
	var req struct {
		Parts []struct {
			PartNumber    int    `xml:"PartNumber"`
			ETag          string `xml:"ETag"`
			ChecksumCRC32 string `xml:"ChecksumCRC32"`
		} `xml:"Part"`
	}
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		s3Error(w, http.StatusBadRequest, "MalformedXML", err.Error())
		return
	}
	f.mu.Lock()
	before := f.beforeComplete
	f.beforeComplete = nil
	f.mu.Unlock()
	if before != nil {
		before()
	}

	f.mu.Lock()
	if st := f.completeStatus; st != 0 {
		f.completeStatus = 0
		f.mu.Unlock()
		code := "InternalError"
		if st == http.StatusConflict {
			code = "ConditionalRequestConflict"
		}
		s3Error(w, st, code, "injected")
		return
	}
	id := r.URL.Query().Get("uploadId")
	up := f.uploads[id]
	if up == nil || up.done {
		f.mu.Unlock()
		s3Error(w, http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist")
		return
	}
	if f.conditional && r.Header.Get("If-None-Match") == "*" && f.objects[key] != nil {
		f.mu.Unlock()
		s3Error(w, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold")
		return
	}
	var data []byte
	var sums []string
	for i, p := range req.Parts {
		got, ok := up.parts[p.PartNumber]
		switch {
		case !ok || got.etag != p.ETag || got.crc != p.ChecksumCRC32:
			f.mu.Unlock()
			s3Error(w, http.StatusBadRequest, "InvalidPart", fmt.Sprintf("part %d is not the part that was sent", p.PartNumber))
			return
		case i < len(req.Parts)-1 && len(got.data) < 5<<20:
			f.mu.Unlock()
			s3Error(w, http.StatusBadRequest, "EntityTooSmall", "Your proposed upload is smaller than the minimum allowed object size.")
			return
		}
		data = append(data, got.data...)
		sums = append(sums, got.crc)
	}
	call := completeCall{
		ifNoneMatch: r.Header.Get("If-None-Match"), checksumType: r.Header.Get("X-Amz-Checksum-Type"),
		crc32: r.Header.Get("X-Amz-Checksum-Crc32"), mpuObjectSize: r.Header.Get("X-Amz-Mp-Object-Size"),
	}
	stored := compositeOf(sums)
	if up.checksumType == "FULL_OBJECT" {
		stored = crc32Base64(data)
		if call.crc32 != stored {
			f.mu.Unlock()
			s3Error(w, http.StatusBadRequest, "BadDigest", "the full-object CRC32 does not match the parts")
			return
		}
	}
	if call.mpuObjectSize != "" && call.mpuObjectSize != strconv.Itoa(len(data)) {
		f.mu.Unlock()
		s3Error(w, http.StatusBadRequest, "InvalidRequest", "the object is not the size the request says")
		return
	}
	versions := 1
	if o := f.objects[key]; o != nil {
		versions = o.versions + 1
	}
	f.objects[key] = &fakeObject{data: data, crc32: stored, versions: versions}
	up.done = true
	f.completes = append(f.completes, call)
	drop := f.dropCompleteAfterCommit
	f.dropCompleteAfterCommit = false
	f.mu.Unlock()
	if drop {
		dropConnection(w)
		return
	}
	w.Header().Set("X-Amz-Version-Id", fmt.Sprintf("v%d", versions))
	writeXML(w, struct {
		XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
		Bucket  string   `xml:"Bucket"`
		Key     string   `xml:"Key"`
		ETag    string   `xml:"ETag"`
	}{Bucket: "backups", Key: key, ETag: fmt.Sprintf(`"multipart-%d"`, len(req.Parts))})
}

// compositeOf is a composite CRC32 as S3 reports it: the CRC32 of the parts' CRC32s, a dash, the
// count of parts.
func compositeOf(sums []string) string {
	var raw []byte
	for _, s := range sums {
		b, _ := base64.StdEncoding.DecodeString(s)
		raw = append(raw, b...)
	}
	return crc32Base64(raw) + "-" + strconv.Itoa(len(sums))
}

func writeXML(w http.ResponseWriter, v any) {
	body, err := xml.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write(append([]byte(xml.Header), body...))
}

func (f *fakeStore) op(name string) {
	f.mu.Lock()
	f.ops[name]++
	f.mu.Unlock()
}

func (f *fakeStore) head(w http.ResponseWriter, r *http.Request, key string) {
	f.mu.Lock()
	f.ops["HeadObject"]++
	hook := f.beforeHead[f.ops["HeadObject"]]
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	o := f.object(key)
	if o == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
	w.Header().Set("ETag", `"`+o.crc32+`"`)
	if r.Header.Get("X-Amz-Checksum-Mode") == "ENABLED" {
		w.Header().Set("X-Amz-Checksum-Crc32", o.crc32)
	}
	w.WriteHeader(http.StatusOK)
}

func (f *fakeStore) putObject(w http.ResponseWriter, r *http.Request, key string) {
	f.op("PutObject")
	got, ok := readWrite(w, r, key)
	if !ok {
		return
	}
	f.mu.Lock()
	existing := f.objects[key]
	if f.conditional && r.Header.Get("If-None-Match") == "*" && existing != nil {
		f.mu.Unlock()
		s3Error(w, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold")
		return
	}
	versions := 1
	if existing != nil {
		versions = existing.versions + 1
	}
	f.objects[key] = &fakeObject{data: got.data, crc32: crc32Base64(got.data), versions: versions}
	f.accepted = append(f.accepted, got)
	drop := f.dropAfterCommit > 0
	if drop {
		f.dropAfterCommit--
	}
	f.mu.Unlock()
	if drop {
		dropConnection(w)
		return
	}
	w.Header().Set("ETag", `"etag-`+key+`"`)
	w.Header().Set("X-Amz-Version-Id", fmt.Sprintf("v%d", versions))
	w.WriteHeader(http.StatusOK)
}

// readWrite reads a write's body, decoding aws-chunked, and checks its checksum. It answers the
// request itself and reports false when the write is refused.
func readWrite(w http.ResponseWriter, r *http.Request, key string) (received, bool) {
	got := received{
		key:         key,
		encoding:    r.Header.Get("Content-Encoding"),
		length:      r.ContentLength,
		payloadHash: r.Header.Get("X-Amz-Content-Sha256"),
		crc32Header: r.Header.Get("X-Amz-Checksum-Crc32"),
		lockMode:    r.Header.Get("X-Amz-Object-Lock-Mode"),
		retainUntil: r.Header.Get("X-Amz-Object-Lock-Retain-Until-Date"),
	}
	want := got.crc32Header
	if strings.Contains(got.encoding, "aws-chunked") {
		data, trailers, err := decodeChunked(r.Body)
		if err != nil {
			s3Error(w, http.StatusBadRequest, "BadRequest", err.Error())
			return got, false
		}
		got.data, want = data, trailers["x-amz-checksum-crc32"]
	} else {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			s3Error(w, http.StatusBadRequest, "IncompleteBody", err.Error())
			return got, false
		}
		got.data = data
	}
	if want == "" {
		s3Error(w, http.StatusBadRequest, "InvalidRequest", "Content-MD5 OR x-amz-checksum- HTTP header is required")
		return got, false
	}
	if sum := crc32Base64(got.data); sum != want {
		s3Error(w, http.StatusBadRequest, "BadDigest", fmt.Sprintf("the CRC32 you specified, %s, did not match the body's, %s", want, sum))
		return got, false
	}
	return got, true
}

// dropConnection closes the connection with no response, as a lost answer looks to the client.
func dropConnection(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	_ = conn.Close()
}

// decodeChunked reads an unsigned aws-chunked body, refusing a chunk over 16 MiB as MinIO does.
func decodeChunked(body io.Reader) ([]byte, map[string]string, error) {
	br := bufio.NewReader(body)
	var data []byte
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, nil, fmt.Errorf("chunk header: %w", err)
		}
		size, err := strconv.ParseInt(strings.TrimSpace(strings.SplitN(line, ";", 2)[0]), 16, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("chunk size %q: %w", line, err)
		}
		if size > minioMaxChunk {
			return nil, nil, fmt.Errorf("chunk too big: choose chunk size <= 16MiB")
		}
		if size == 0 {
			break
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, nil, fmt.Errorf("chunk data: %w", err)
		}
		data = append(data, chunk...)
		if _, err := br.Discard(2); err != nil {
			return nil, nil, fmt.Errorf("chunk end: %w", err)
		}
	}
	trailers := map[string]string{}
	for {
		line, err := br.ReadString('\n')
		if err != nil || strings.TrimSpace(line) == "" {
			break
		}
		if name, value, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
			trailers[strings.ToLower(name)] = value
		}
	}
	return data, trailers, nil
}

func s3Error(w http.ResponseWriter, status int, code, msg string) {
	body, err := xml.Marshal(struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}{Code: code, Message: msg})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write(append([]byte(xml.Header), body...))
}

func crc32Base64(b []byte) string {
	sum := crc32.NewIEEE()
	_, _ = sum.Write(b)
	return base64.StdEncoding.EncodeToString(sum.Sum(nil))
}

// newBackend points the S3 destination at the fake over TLS, trusting the fake's certificate
// through AWS_CA_BUNDLE as an operator's store would be trusted, and keeps the machine's own AWS
// setup out of it.
func newBackend(t *testing.T, srv *httptest.Server) *s3backend.Backend {
	t.Helper()
	return newBackendWith(t, srv, s3backend.Options{})
}

func newBackendWith(t *testing.T, srv *httptest.Server, opts s3backend.Options) *s3backend.Backend {
	t.Helper()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"AWS_CA_BUNDLE": ca, "AWS_ACCESS_KEY_ID": "AKIDTEST", "AWS_SECRET_ACCESS_KEY": "secret",
		"AWS_SESSION_TOKEN": "", "AWS_PROFILE": "", "AWS_CONFIG_FILE": empty, "AWS_SHARED_CREDENTIALS_FILE": empty,
		"AWS_EC2_METADATA_DISABLED": "true",
	} {
		t.Setenv(k, v)
	}
	opts.Bucket, opts.Region, opts.Endpoint, opts.UsePathStyle = "backups", "us-east-1", srv.URL, true
	b, err := s3backend.New(context.Background(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
