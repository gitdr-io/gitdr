//go:build scale

// Command s3limits sits in front of an S3-compatible store and holds it to AWS's limits, so a
// test against MinIO fails where a write to AWS would. MinIO takes a single PUT of terabytes,
// and a test suite that only ever met MinIO never learned that AWS stops at 5 GiB.
//
// Every request is forwarded unchanged, Host header included, so the client's SigV4 signature
// still holds at the store. Before forwarding, it refuses what AWS refuses:
//   - a PutObject over 5 GiB (EntityTooLarge);
//   - a part number outside 1 to 10,000, or a part over 5 GiB;
//   - a CompleteMultipartUpload where a part other than the last is under 5 MiB (EntityTooSmall);
//   - a write carrying Object Lock headers, or a part of a locked upload, with no checksum.
//
// It answers If-None-Match like AWS (passed on to the store) or like Backblaze B2 (501), throttles
// bodies in both directions, counts requests by operation, and takes faults through an admin API
// under /_s3limits/. Path-style requests only.
//
// One kind of request is changed on the way, so the store behaves like AWS where MinIO does not.
// Over TLS the AWS SDK sends a body of known length as a single aws-chunked chunk. AWS takes a
// chunk of any size and MinIO refuses one over 16 MiB. Such a body is split into smaller chunks
// and, because the new framing changes the signed content length, signed again with the store's
// credentials. The admin API turns this off (rechunkBytes 0) to show what MinIO itself does.
//
// It is part of the scale harness and is built with the scale tag; see scale/README.md.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const (
	gib = int64(1) << 30
	mib = int64(1) << 20
)

func main() {
	listen := flag.String("listen", envOr("S3LIMITS_LISTEN", ":9443"), "address to serve TLS on")
	certFile := flag.String("cert", envOr("S3LIMITS_CERT", ""), "TLS certificate, PEM")
	keyFile := flag.String("key", envOr("S3LIMITS_KEY", ""), "TLS key, PEM")
	upstream := flag.String("upstream", envOr("S3LIMITS_UPSTREAM", ""), "the store, https://host:port")
	upstreamCA := flag.String("upstream-ca", envOr("S3LIMITS_UPSTREAM_CA", ""), "CA bundle for the store's certificate")
	rate := flag.Int64("rate", envInt("S3LIMITS_RATE", 8_000_000), "bytes per second in each direction, 0 for no limit")
	inm := flag.String("if-none-match", envOr("S3LIMITS_IF_NONE_MATCH", "aws"), "aws: pass If-None-Match on; b2: answer it 501")
	region := flag.String("region", envOr("S3LIMITS_REGION", "us-east-1"), "the store's region, for signing a re-chunked request")
	flag.Parse()

	if *certFile == "" || *keyFile == "" || *upstream == "" {
		log.Fatal("s3limits: -cert, -key and -upstream are required")
	}
	pool := x509.NewCertPool()
	if *upstreamCA != "" {
		pem, err := os.ReadFile(*upstreamCA)
		if err != nil {
			log.Fatalf("s3limits: %v", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			log.Fatalf("s3limits: no certificate in %s", *upstreamCA)
		}
	}
	set := defaultSettings()
	set.RateBytesPerSecond = *rate
	set.IfNoneMatch = *inm
	p, err := newProxy(*upstream, pool, set)
	if err != nil {
		log.Fatalf("s3limits: %v", err)
	}
	// The store's own credentials, from the environment only, for signing a re-chunked body.
	p.creds = aws.Credentials{AccessKeyID: os.Getenv("S3LIMITS_ACCESS_KEY"), SecretAccessKey: os.Getenv("S3LIMITS_SECRET_KEY")}
	p.region = *region
	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 30 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
		// S3 speaks HTTP/1.1, and a dropped response has to look the way it does there.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("s3limits: %v", err)
	}
	log.Printf("s3limits: serving %s for %s, %d bytes/s, if-none-match %s", ln.Addr(), *upstream, *rate, *inm)
	log.Fatal(srv.ServeTLS(ln, *certFile, *keyFile))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// settings are the limits in force. The admin API replaces them whole.
type settings struct {
	MaxPutBytes  int64 `json:"maxPutBytes"`
	MaxPartBytes int64 `json:"maxPartBytes"`
	MinPartBytes int64 `json:"minPartBytes"`
	MaxParts     int   `json:"maxParts"`
	// LockChecksum refuses a locked write that carries no checksum, as AWS does.
	LockChecksum bool `json:"lockChecksum"`
	// IfNoneMatch is "aws", which passes the header on, or "b2", which answers 501.
	IfNoneMatch        string `json:"ifNoneMatch"`
	RateBytesPerSecond int64  `json:"rateBytesPerSecond"`
	// RechunkBytes is the chunk size a single-chunk aws-chunked body is split into. 0 passes it
	// on as it came.
	RechunkBytes int64 `json:"rechunkBytes"`
}

func defaultSettings() settings {
	return settings{
		MaxPutBytes:        5 * gib,
		MaxPartBytes:       5 * gib,
		MinPartBytes:       5 * mib,
		MaxParts:           10_000,
		LockChecksum:       true,
		IfNoneMatch:        "aws",
		RateBytesPerSecond: 8_000_000,
		RechunkBytes:       8 * mib,
	}
}

// opStats counts one operation.
type opStats struct {
	Count    int64            `json:"count"`
	BytesIn  int64            `json:"bytesIn"`
	BytesOut int64            `json:"bytesOut"`
	Status   map[string]int64 `json:"status"`
}

// stats is everything counted since the proxy started. Counters only grow; a caller that wants
// one phase takes the difference of two snapshots.
type stats struct {
	Ops map[string]*opStats `json:"ops"`
	// Refused counts the requests answered here, by S3 error code, without reaching the store.
	Refused map[string]int64 `json:"refused"`
	// BytesResent is the body bytes of writes to a key that had already been written once.
	BytesResent  int64    `json:"bytesResent"`
	WritesResent int64    `json:"writesResent"`
	ResentSample []string `json:"resentSample,omitempty"`
	// OpenUploads is the multipart uploads created and neither completed nor aborted.
	OpenUploads int64 `json:"openUploads"`
	// Rechunked is the writes whose single chunk was split for the store.
	Rechunked int64 `json:"rechunked"`
}

// fault is one injected failure. A request matches when its operation, bucket and key match;
// the fault fires on the nth match and on Times matches from there (-1 for every one).
type fault struct {
	ID       int    `json:"id"`
	Op       string `json:"op,omitempty"`
	Bucket   string `json:"bucket,omitempty"`
	KeyRegex string `json:"keyRegex,omitempty"`
	Nth      int    `json:"nth,omitempty"`
	Times    int    `json:"times,omitempty"`
	// Action is "status" (answer Status with Code, never reaching the store), "drop-after-commit"
	// (forward, then close the connection without a response), "hold-before" (wait for a release
	// before forwarding) or "hold-after" (forward, then wait for a release before answering).
	Action string `json:"action"`
	Status int    `json:"status,omitempty"`
	Code   string `json:"code,omitempty"`

	Matched int `json:"matched"`
	Fired   int `json:"fired"`
	Holding int `json:"holding"`

	re      *regexp.Regexp
	release chan struct{}
}

type upload struct {
	key    string
	locked bool
	parts  map[int]int64
}

type proxy struct {
	upstream  *url.URL
	transport *http.Transport
	creds     aws.Credentials
	region    string
	signer    *v4.Signer

	mu      sync.Mutex
	set     settings
	in, out *throttle
	st      stats
	written map[string]bool // bucket/key written at least once
	uploads map[string]*upload
	faults  []*fault
	nextID  int
}

func newProxy(upstream string, pool *x509.CertPool, set settings) (*proxy, error) {
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("upstream %q is not an http(s) URL", upstream)
	}
	p := &proxy{
		upstream: u,
		transport: &http.Transport{
			TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
			// The client chose its encoding and signed for it. Asking the store for gzip would
			// change what comes back.
			DisableCompression: true,
		},
		region:  "us-east-1",
		signer:  v4.NewSigner(),
		set:     set,
		in:      newThrottle(set.RateBytesPerSecond),
		out:     newThrottle(set.RateBytesPerSecond),
		st:      stats{Ops: map[string]*opStats{}, Refused: map[string]int64{}},
		written: map[string]bool{},
		uploads: map[string]*upload{},
	}
	return p, nil
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_s3limits/") {
		p.admin(w, r)
		return
	}
	op, bucket, key := classify(r)

	p.mu.Lock()
	set := p.set
	p.mu.Unlock()

	if code, status, msg := p.refuse(r, op, bucket, set); code != "" {
		p.count(op, status, 0, 0)
		p.mu.Lock()
		p.st.Refused[code]++
		p.mu.Unlock()
		writeError(w, status, code, msg)
		return
	}

	// The body of a Complete is read here: its part list is checked before it goes anywhere.
	var completeBody []byte
	if op == "CompleteMultipartUpload" {
		b, err := io.ReadAll(io.LimitReader(r.Body, 16*mib))
		if err != nil {
			writeError(w, http.StatusBadRequest, "IncompleteBody", err.Error())
			return
		}
		completeBody = b
		if code, msg := p.checkComplete(r.URL.Query().Get("uploadId"), b, set); code != "" {
			p.count(op, http.StatusBadRequest, int64(len(b)), 0)
			p.mu.Lock()
			p.st.Refused[code]++
			p.mu.Unlock()
			writeError(w, http.StatusBadRequest, code, msg)
			return
		}
	}

	f := p.match(op, bucket, key)
	if f != nil && f.Action == "status" {
		status := f.Status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		code := f.Code
		if code == "" {
			code = "InternalError"
		}
		p.count(op, status, 0, 0)
		writeError(w, status, code, "injected by s3limits")
		return
	}
	if f != nil && f.Action == "hold-before" {
		if !p.hold(r, f) {
			return // the client went away while it was held
		}
	}

	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme = p.upstream.Scheme
	out.URL.Host = p.upstream.Host
	out.Host = r.Host // signed by the client
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	var bodyIn counter
	switch {
	case completeBody != nil:
		out.Body = io.NopCloser(bytes.NewReader(completeBody))
		out.ContentLength = int64(len(completeBody))
		bodyIn.n = int64(len(completeBody))
	case r.ContentLength == 0:
		out.Body = http.NoBody
	default:
		body, length, resign, err := p.rechunk(r, op, &countingReader{r: p.in.reader(r.Body), c: &bodyIn}, set)
		if err != nil {
			writeError(w, http.StatusBadRequest, "IncompleteBody", err.Error())
			return
		}
		out.Body, out.ContentLength = io.NopCloser(body), length
		if resign {
			if err := p.sign(out); err != nil {
				writeError(w, http.StatusInternalServerError, "InternalError", "s3limits: signing: "+err.Error())
				return
			}
			p.mu.Lock()
			p.st.Rechunked++
			p.mu.Unlock()
		}
	}

	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		p.count(op, http.StatusBadGateway, bodyIn.get(), 0)
		if r.Context().Err() != nil {
			return
		}
		writeError(w, http.StatusBadGateway, "InternalError", "s3limits: upstream: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	p.afterWrite(op, bucket, key, r, resp.StatusCode, bodyIn.get())

	// The body of a CreateMultipartUpload names the upload, which the part checks need. It is read
	// here and then sent on as the store's body would have been.
	var respBody io.Reader = resp.Body
	if op == "CreateMultipartUpload" && resp.StatusCode/100 == 2 {
		created, err := io.ReadAll(io.LimitReader(resp.Body, mib))
		if err != nil {
			writeError(w, http.StatusBadGateway, "InternalError", "s3limits: upstream body: "+err.Error())
			return
		}
		p.recordUpload(r, key, created)
		respBody = bytes.NewReader(created)
	}

	if f != nil && f.Action == "drop-after-commit" {
		_, _ = io.Copy(io.Discard, resp.Body)
		p.count(op, 0, bodyIn.get(), 0)
		// Closes the connection with no response. The write has landed, and the client cannot know.
		panic(http.ErrAbortHandler)
	}
	if f != nil && f.Action == "hold-after" {
		if !p.hold(r, f) {
			p.count(op, resp.StatusCode, bodyIn.get(), 0)
			return
		}
	}

	for k, vs := range resp.Header {
		if slices.Contains(hopHeaders, http.CanonicalHeaderKey(k)) {
			continue
		}
		w.Header()[k] = vs
	}
	w.WriteHeader(resp.StatusCode)
	var bodyOut counter
	_, _ = io.Copy(w, &countingReader{r: p.out.reader(respBody), c: &bodyOut})
	p.count(op, resp.StatusCode, bodyIn.get(), bodyOut.get())
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Upgrade", "Expect",
}

// rechunk splits a body sent as one aws-chunked chunk into chunks of set.RechunkBytes, and
// reports the new content length and whether the request has to be signed again. Anything else
// goes on as it came: a signed stream, whose chunks carry signatures, a body already in small
// chunks, or a proxy with no credentials to sign with.
func (p *proxy) rechunk(r *http.Request, op string, body io.Reader, set settings) (io.Reader, int64, bool, error) {
	if set.RechunkBytes <= 0 || p.creds.AccessKeyID == "" || (op != "PutObject" && op != "UploadPart") ||
		r.Header.Get("X-Amz-Content-Sha256") != "STREAMING-UNSIGNED-PAYLOAD-TRAILER" {
		return body, r.ContentLength, false, nil
	}
	size := objectSize(r)
	if size <= set.RechunkBytes {
		return body, r.ContentLength, false, nil
	}
	br := bufio.NewReaderSize(body, 64<<10)
	head, _ := br.Peek(20)
	i := bytes.Index(head, []byte("\r\n"))
	if i <= 0 {
		return br, r.ContentLength, false, nil
	}
	if n, err := strconv.ParseInt(string(head[:i]), 16, 64); err != nil || n != size {
		return br, r.ContentLength, false, nil
	}
	if _, err := br.Discard(i + 2); err != nil {
		return nil, 0, false, err
	}
	length := r.ContentLength - int64(i) - 4 + chunkFraming(size, set.RechunkBytes)
	pr, pw := io.Pipe()
	go func() { _ = pw.CloseWithError(writeChunks(pw, br, size, set.RechunkBytes)) }()
	return pr, length, true, nil
}

// chunkFraming is the bytes aws-chunked adds around size bytes of data in chunks of chunk: the
// hex length and a CRLF before each chunk, and a CRLF after it.
func chunkFraming(size, chunk int64) int64 {
	var n int64
	for left := size; left > 0; left -= chunk {
		n += int64(len(strconv.FormatInt(min(left, chunk), 16))) + 4
	}
	return n
}

// writeChunks copies size bytes of data from r to w in chunks of chunk. The CRLF that ended the
// one big chunk is dropped, and the rest, the final chunk and the trailers, goes as it came.
func writeChunks(w io.Writer, r *bufio.Reader, size, chunk int64) error {
	for left := size; left > 0; {
		c := min(left, chunk)
		if _, err := io.WriteString(w, strconv.FormatInt(c, 16)+"\r\n"); err != nil {
			return err
		}
		if _, err := io.CopyN(w, r, c); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\r\n"); err != nil {
			return err
		}
		left -= c
	}
	var crlf [2]byte
	if _, err := io.ReadFull(r, crlf[:]); err != nil || string(crlf[:]) != "\r\n" {
		return errors.New("aws-chunked: the chunk does not end where its length says")
	}
	_, err := io.Copy(w, r)
	return err
}

// sign signs req for the store with the proxy's own credentials, the way the SDK signs for S3.
func (p *proxy) sign(req *http.Request) error {
	req.Header.Del("Authorization")
	req.Header.Del("X-Amz-Date")
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	return p.signer.SignHTTP(ctx, p.creds, req, req.Header.Get("X-Amz-Content-Sha256"), "s3", p.region, time.Now(),
		func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
}

// refuse answers what AWS would refuse before reading the body, or "" to let it through.
func (p *proxy) refuse(r *http.Request, op, bucket string, set settings) (code string, status int, msg string) {
	if r.Header.Get("If-None-Match") != "" && set.IfNoneMatch == "b2" {
		return "NotImplemented", http.StatusNotImplemented, "A header you provided implies functionality that is not implemented"
	}
	switch op {
	case "PutObject":
		if size := objectSize(r); size > set.MaxPutBytes {
			return "EntityTooLarge", http.StatusBadRequest, fmt.Sprintf(
				"Your proposed upload exceeds the maximum allowed size: %d bytes, the limit is %d", size, set.MaxPutBytes)
		}
		if set.LockChecksum && locked(r.Header) && !hasChecksum(r.Header) {
			return "InvalidRequest", http.StatusBadRequest,
				"Content-MD5 OR x-amz-checksum- HTTP header is required for Put Object requests with Object Lock parameters"
		}
	case "UploadPart":
		n, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
		if err != nil || n < 1 || n > set.MaxParts {
			return "InvalidArgument", http.StatusBadRequest,
				fmt.Sprintf("Part number must be an integer between 1 and %d, inclusive", set.MaxParts)
		}
		if size := objectSize(r); size > set.MaxPartBytes {
			return "EntityTooLarge", http.StatusBadRequest, fmt.Sprintf(
				"Your proposed upload exceeds the maximum allowed size: %d bytes, the limit is %d", size, set.MaxPartBytes)
		}
		p.mu.Lock()
		u := p.uploads[r.URL.Query().Get("uploadId")]
		p.mu.Unlock()
		if set.LockChecksum && u != nil && u.locked && !hasChecksum(r.Header) {
			return "InvalidRequest", http.StatusBadRequest,
				"Content-MD5 OR x-amz-checksum- HTTP header is required for Put Part requests with Object Lock parameters"
		}
	}
	_ = bucket
	return "", 0, ""
}

type completeRequest struct {
	Parts []struct {
		PartNumber int `xml:"PartNumber"`
	} `xml:"Part"`
}

// checkComplete holds a part list to AWS's rules: at most MaxParts parts, and every part but the
// last at least MinPartBytes. A part this proxy never saw is left for the store to refuse.
func (p *proxy) checkComplete(uploadID string, body []byte, set settings) (code, msg string) {
	var req completeRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		return "MalformedXML", "The XML you provided was not well-formed: " + err.Error()
	}
	if len(req.Parts) > set.MaxParts {
		return "InvalidArgument", fmt.Sprintf("%d parts, the limit is %d", len(req.Parts), set.MaxParts)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	u := p.uploads[uploadID]
	if u == nil {
		return "", ""
	}
	last := 0
	for _, part := range req.Parts {
		last = max(last, part.PartNumber)
	}
	for _, part := range req.Parts {
		size, seen := u.parts[part.PartNumber]
		if seen && part.PartNumber != last && size < set.MinPartBytes {
			return "EntityTooSmall", fmt.Sprintf(
				"Your proposed upload is smaller than the minimum allowed object size: part %d is %d bytes, the minimum is %d",
				part.PartNumber, size, set.MinPartBytes)
		}
	}
	return "", ""
}

func (p *proxy) recordUpload(r *http.Request, key string, body []byte) {
	var res struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(body, &res); err != nil || res.UploadID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.uploads[res.UploadID] = &upload{key: key, locked: locked(r.Header), parts: map[int]int64{}}
	p.st.OpenUploads++
}

// afterWrite records what a write did once the store has answered it.
func (p *proxy) afterWrite(op, bucket, key string, r *http.Request, status int, bytesIn int64) {
	ok := status/100 == 2
	p.mu.Lock()
	defer p.mu.Unlock()
	switch op {
	case "PutObject", "CompleteMultipartUpload":
		id := bucket + "/" + key
		if p.written[id] {
			p.st.WritesResent++
			p.st.BytesResent += bytesIn
			if len(p.st.ResentSample) < 20 {
				p.st.ResentSample = append(p.st.ResentSample, id)
			}
		}
		if ok {
			p.written[id] = true
		}
		if op == "CompleteMultipartUpload" && ok {
			if _, open := p.uploads[r.URL.Query().Get("uploadId")]; open {
				delete(p.uploads, r.URL.Query().Get("uploadId"))
				p.st.OpenUploads--
			}
		}
	case "UploadPart":
		if u := p.uploads[r.URL.Query().Get("uploadId")]; u != nil && ok {
			n, _ := strconv.Atoi(r.URL.Query().Get("partNumber"))
			u.parts[n] = objectSize(r)
		}
	case "AbortMultipartUpload":
		if _, open := p.uploads[r.URL.Query().Get("uploadId")]; open && ok {
			delete(p.uploads, r.URL.Query().Get("uploadId"))
			p.st.OpenUploads--
		}
	}
}

func (p *proxy) count(op string, status int, in, out int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.st.Ops[op]
	if s == nil {
		s = &opStats{Status: map[string]int64{}}
		p.st.Ops[op] = s
	}
	s.Count++
	s.BytesIn += in
	s.BytesOut += out
	label := strconv.Itoa(status)
	if status == 0 {
		label = "dropped"
	}
	s.Status[label]++
}

// match returns the fault this request fires, or nil.
func (p *proxy) match(op, bucket, key string) *fault {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.faults {
		if f.Op != "" && f.Op != op {
			continue
		}
		if f.Bucket != "" && f.Bucket != bucket {
			continue
		}
		if f.re != nil && !f.re.MatchString(key) {
			continue
		}
		f.Matched++
		nth := max(f.Nth, 1)
		times := f.Times
		if times == 0 {
			times = 1
		}
		if f.Matched < nth || (times > 0 && f.Fired >= times) {
			continue
		}
		f.Fired++
		return f
	}
	return nil
}

// hold waits until f is released or the client goes away, and reports whether the client is
// still there.
func (p *proxy) hold(r *http.Request, f *fault) bool {
	p.mu.Lock()
	f.Holding++
	release := f.release
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		f.Holding--
		p.mu.Unlock()
	}()
	select {
	case <-release:
		return r.Context().Err() == nil
	case <-r.Context().Done():
		return false
	}
}

func (p *proxy) admin(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/_s3limits/healthz" && r.Method == http.MethodGet:
		p.health(w, r)
	case r.URL.Path == "/_s3limits/stats" && r.Method == http.MethodGet:
		p.mu.Lock()
		b, err := json.Marshal(p.st)
		p.mu.Unlock()
		writeJSON(w, b, err)
	case r.URL.Path == "/_s3limits/settings" && r.Method == http.MethodGet:
		p.mu.Lock()
		b, err := json.Marshal(p.set)
		p.mu.Unlock()
		writeJSON(w, b, err)
	case r.URL.Path == "/_s3limits/settings" && r.Method == http.MethodPut:
		var s settings
		if err := json.NewDecoder(io.LimitReader(r.Body, mib)).Decode(&s); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if s.IfNoneMatch != "aws" && s.IfNoneMatch != "b2" {
			http.Error(w, `ifNoneMatch is "aws" or "b2"`, http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.set = s
		p.in.setRate(s.RateBytesPerSecond)
		p.out.setRate(s.RateBytesPerSecond)
		p.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/_s3limits/faults" && r.Method == http.MethodGet:
		p.mu.Lock()
		b, err := json.Marshal(p.faults)
		p.mu.Unlock()
		writeJSON(w, b, err)
	case r.URL.Path == "/_s3limits/faults" && r.Method == http.MethodPost:
		p.addFault(w, r)
	case r.URL.Path == "/_s3limits/faults" && r.Method == http.MethodDelete:
		p.mu.Lock()
		for _, f := range p.faults {
			close(f.release)
		}
		p.faults = nil
		p.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/_s3limits/faults/release" && r.Method == http.MethodPost:
		p.mu.Lock()
		for _, f := range p.faults {
			close(f.release)
			f.release = make(chan struct{})
		}
		p.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (p *proxy) addFault(w http.ResponseWriter, r *http.Request) {
	var f fault
	if err := json.NewDecoder(io.LimitReader(r.Body, mib)).Decode(&f); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch f.Action {
	case "status", "drop-after-commit", "hold-before", "hold-after":
	default:
		http.Error(w, "action is status, drop-after-commit, hold-before or hold-after", http.StatusBadRequest)
		return
	}
	if f.KeyRegex != "" {
		re, err := regexp.Compile(f.KeyRegex)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.re = re
	}
	f.release = make(chan struct{})
	p.mu.Lock()
	p.nextID++
	f.ID = p.nextID
	f.Matched, f.Fired, f.Holding = 0, 0, 0
	p.faults = append(p.faults, &f)
	b, err := json.Marshal(map[string]int{"id": f.ID})
	p.mu.Unlock()
	writeJSON(w, b, err)
}

// health answers 200 once the store answers its own liveness probe.
func (p *proxy) health(w http.ResponseWriter, r *http.Request) {
	u := *p.upstream
	u.Path = "/minio/health/live"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "upstream answered "+resp.Status, http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func writeJSON(w http.ResponseWriter, b []byte, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

type s3Error struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	RequestID string   `xml:"RequestId"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	b, err := xml.Marshal(s3Error{Code: code, Message: msg, RequestID: "s3limits"})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)
	_, _ = w.Write(append([]byte(xml.Header), b...))
}

// classify names the S3 operation of a path-style request.
func classify(r *http.Request) (op, bucket, key string) {
	rest := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ = strings.Cut(rest, "/")
	q := r.URL.Query()
	has := func(k string) bool { _, ok := q[k]; return ok }
	switch r.Method {
	case http.MethodPut:
		switch {
		case bucket == "":
			op = "PUT"
		case key == "" && has("object-lock"):
			op = "PutObjectLockConfiguration"
		case key == "" && has("versioning"):
			op = "PutBucketVersioning"
		case key == "" && has("lifecycle"):
			op = "PutBucketLifecycleConfiguration"
		case key == "":
			op = "CreateBucket"
		case has("partNumber") && has("uploadId"):
			op = "UploadPart"
			if r.Header.Get("X-Amz-Copy-Source") != "" {
				op = "UploadPartCopy"
			}
		case has("retention"):
			op = "PutObjectRetention"
		case has("legal-hold"):
			op = "PutObjectLegalHold"
		case has("tagging"):
			op = "PutObjectTagging"
		case r.Header.Get("X-Amz-Copy-Source") != "":
			op = "CopyObject"
		default:
			op = "PutObject"
		}
	case http.MethodPost:
		switch {
		case has("uploads"):
			op = "CreateMultipartUpload"
		case has("uploadId"):
			op = "CompleteMultipartUpload"
		case has("delete"):
			op = "DeleteObjects"
		default:
			op = "POST"
		}
	case http.MethodGet:
		switch {
		case bucket == "":
			op = "ListBuckets"
		case key == "" && has("object-lock"):
			op = "GetObjectLockConfiguration"
		case key == "" && has("uploads"):
			op = "ListMultipartUploads"
		case key == "" && has("versions"):
			op = "ListObjectVersions"
		case key == "" && has("location"):
			op = "GetBucketLocation"
		case key == "" && has("versioning"):
			op = "GetBucketVersioning"
		case key == "" && q.Get("list-type") == "2":
			op = "ListObjectsV2"
		case key == "":
			op = "ListObjects"
		case has("retention"):
			op = "GetObjectRetention"
		case has("legal-hold"):
			op = "GetObjectLegalHold"
		case has("uploadId"):
			op = "ListParts"
		case has("tagging"):
			op = "GetObjectTagging"
		default:
			op = "GetObject"
		}
	case http.MethodHead:
		op = "HeadObject"
		if key == "" {
			op = "HeadBucket"
		}
	case http.MethodDelete:
		switch {
		case key == "":
			op = "DeleteBucket"
		case has("uploadId"):
			op = "AbortMultipartUpload"
		default:
			op = "DeleteObject"
		}
	default:
		op = r.Method
	}
	return op, bucket, key
}

// objectSize is the size of the object a write carries: the decoded length of an aws-chunked
// body, which is what the SDK sends over TLS, or the content length.
func objectSize(r *http.Request) int64 {
	if v := r.Header.Get("X-Amz-Decoded-Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return r.ContentLength
}

func locked(h http.Header) bool {
	return h.Get("X-Amz-Object-Lock-Mode") != "" || h.Get("X-Amz-Object-Lock-Retain-Until-Date") != "" ||
		h.Get("X-Amz-Object-Lock-Legal-Hold") != ""
}

// hasChecksum reports whether a write carries an integrity value: Content-MD5, an
// x-amz-checksum-* header, or a trailer that will carry one.
func hasChecksum(h http.Header) bool {
	if h.Get("Content-Md5") != "" {
		return true
	}
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-checksum-") {
			return true
		}
	}
	return strings.Contains(strings.ToLower(h.Get("X-Amz-Trailer")), "x-amz-checksum-")
}

type counter struct {
	mu sync.Mutex
	n  int64
}

func (c *counter) add(n int) {
	c.mu.Lock()
	c.n += int64(n)
	c.mu.Unlock()
}

func (c *counter) get() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

type countingReader struct {
	r io.Reader
	c *counter
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.c.add(n)
	return n, err
}

// throttle is a token bucket shared by every connection in one direction, the way one uplink is.
type throttle struct {
	mu     sync.Mutex
	rate   int64
	tokens float64
	last   time.Time
}

func newThrottle(rate int64) *throttle { return &throttle{rate: rate, last: time.Now()} }

func (t *throttle) setRate(rate int64) {
	t.mu.Lock()
	t.rate = rate
	t.mu.Unlock()
}

// wait blocks until n bytes may pass.
func (t *throttle) wait(n int) {
	t.mu.Lock()
	if t.rate <= 0 {
		t.mu.Unlock()
		return
	}
	now := time.Now()
	t.tokens += now.Sub(t.last).Seconds() * float64(t.rate)
	// At most a quarter of a second banked, so an idle gap does not become a burst.
	t.tokens = min(t.tokens, float64(t.rate)/4)
	t.last = now
	t.tokens -= float64(n)
	var sleep time.Duration
	if t.tokens < 0 {
		sleep = time.Duration(-t.tokens / float64(t.rate) * float64(time.Second))
	}
	t.mu.Unlock()
	if sleep > 0 {
		time.Sleep(sleep)
	}
}

func (t *throttle) reader(r io.Reader) io.Reader { return &throttledReader{r: r, t: t} }

type throttledReader struct {
	r io.Reader
	t *throttle
}

func (tr *throttledReader) Read(b []byte) (int, error) {
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	n, err := tr.r.Read(b)
	if n > 0 {
		tr.t.wait(n)
	}
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	return n, err
}
