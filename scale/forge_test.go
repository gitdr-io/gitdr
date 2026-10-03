//go:build scale

package scale

import (
	"bytes"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/cgi"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fakeforge: the part of GitHub the engine talks to, served from bare repositories on disk.
//
//   - The REST subset the GitHub source calls: an App installation token, the installation's
//     repositories in pages of up to 100, and each repository's metadata lists. Every request
//     spends from the installation's rate-limit budget and carries GitHub's X-RateLimit headers;
//     a spent budget answers 403 until the window resets, as GitHub does.
//   - git over smart HTTP, through `git http-backend` as a CGI, with the installation token as
//     the password, the way GitHub takes it.
//   - The LFS batch API, answering with download links that expire.
//
// It runs inside the test process, so a scenario can change a repository between simulated days,
// spend a budget or hold a clone, without an API of its own for any of it.

type forge struct {
	h          *harness
	srv        *http.Server
	base       string // http://127.0.0.1:port
	root       string // repositories, root/owner/name.git
	lfsRoot    string // LFS objects, lfsRoot/owner/name/oid
	cgi        *cgi.Handler
	appID      int64
	appKeyPath string
	appKeyPEM  []byte
	appPub     *rsa.PublicKey
	linkKey    []byte

	mu       sync.Mutex
	installs map[int64]*installation
	repos    map[string]*forgeRepo
	tokens   map[string]grant
	holds    []*fetchHold
	st       forgeStats
	// tokenTTL is how long an installation token lasts; GitHub's is an hour. linkTTL is how long
	// an LFS download link lasts.
	tokenTTL time.Duration
	linkTTL  time.Duration
}

type installation struct {
	id    int64
	repos []string // slugs, in listing order
	// limit REST requests in each window. 0 is a budget no run spends, so that only the scenario
	// about rate limits meets one: day 1 of 2,500 repositories makes about 20,000 requests, and
	// GitHub gives an installation 5,000 to 12,500 an hour.
	limit  int
	window time.Duration
	used   int
	reset  time.Time
}

type forgeRepo struct {
	owner, name string
	id          int64
	sizeKB      int64
	meta        repoMeta
}

// repoMeta is what the metadata endpoints list for a repository.
type repoMeta struct {
	Labels, Milestones, Issues, Comments, Pulls, ReviewComments, Releases []map[string]any
}

type grant struct {
	install int64
	expires time.Time
}

type forgeStats struct {
	Requests map[string]int64 `json:"requests"`
	// BudgetSpent counts the requests that spent the last of a budget. go-github stops sending by
	// itself once a response says nothing is left, so a spent budget is often never refused.
	BudgetSpent     int64 `json:"budgetSpent"`
	RateLimited     int64 `json:"rateLimited"`
	Unauthorized    int64 `json:"unauthorized"`
	TokensMinted    int64 `json:"tokensMinted"`
	UploadPackBytes int64 `json:"uploadPackBytes"`
	LFSBytes        int64 `json:"lfsBytes"`
}

func (s forgeStats) clone() forgeStats {
	s.Requests = maps.Clone(s.Requests)
	return s
}

func startForge(h *harness) (*forge, error) {
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		return nil, fmt.Errorf("git --exec-path: %w", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		return nil, fmt.Errorf("git-http-backend: %w", err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	f := &forge{
		h:        h,
		root:     filepath.Join(h.root, "forge", "git"),
		lfsRoot:  filepath.Join(h.root, "forge", "lfs"),
		appID:    4242,
		appPub:   &key.PublicKey,
		linkKey:  []byte(randomHex(32)),
		installs: map[int64]*installation{},
		repos:    map[string]*forgeRepo{},
		tokens:   map[string]grant{},
		st:       forgeStats{Requests: map[string]int64{}},
		tokenTTL: time.Hour,
		linkTTL:  time.Hour,
	}
	f.appKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	f.appKeyPath = filepath.Join(h.root, "github-app.pem")
	if err := os.WriteFile(f.appKeyPath, f.appKeyPEM, 0o600); err != nil {
		return nil, err
	}
	for _, d := range []string{f.root, f.lfsRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	cgiLog, err := os.Create(filepath.Join(h.logDir, "forge-http-backend.log"))
	if err != nil {
		return nil, err
	}
	f.cgi = &cgi.Handler{
		Path: backend,
		Root: "/",
		Dir:  f.root,
		Env: []string{
			"GIT_PROJECT_ROOT=" + f.root,
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_NOSYSTEM=1",
			"HOME=" + os.Getenv("HOME"),
		},
		InheritEnv: []string{"PATH"},
		Stderr:     cgiLog,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v3/app/installations/{id}/access_tokens", f.accessToken)
	mux.HandleFunc("GET /api/v3/installation/repositories", f.api("installation/repositories", f.installationRepos))
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}", f.api("repos/get", f.getRepo))
	for path, pick := range map[string]func(repoMeta) []map[string]any{
		"labels":          func(m repoMeta) []map[string]any { return m.Labels },
		"milestones":      func(m repoMeta) []map[string]any { return m.Milestones },
		"issues":          func(m repoMeta) []map[string]any { return m.Issues },
		"issues/comments": func(m repoMeta) []map[string]any { return m.Comments },
		"pulls":           func(m repoMeta) []map[string]any { return m.Pulls },
		"pulls/comments":  func(m repoMeta) []map[string]any { return m.ReviewComments },
		"releases":        func(m repoMeta) []map[string]any { return m.Releases },
	} {
		mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/"+path, f.api("repos/"+path, f.listMeta(pick)))
	}
	mux.HandleFunc("GET /{owner}/{repo}/info/refs", f.git)
	mux.HandleFunc("POST /{owner}/{repo}/git-upload-pack", f.git)
	mux.HandleFunc("POST /{owner}/{repo}/info/lfs/objects/batch", f.lfsBatch)
	mux.HandleFunc("GET /_lfs/{owner}/{repo}/objects/{oid}", f.lfsObject)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f.base = "http://" + ln.Addr().String()
	f.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = f.srv.Serve(ln) }()
	fmt.Fprintf(os.Stderr, "scale: fake forge at %s\n", f.base)
	return f, nil
}

func (f *forge) close() { _ = f.srv.Close() }

// apiURL is what the engine is configured with as source.baseURL.
func (f *forge) apiURL() string { return f.base + "/api/v3" }

// host is how the engine records this forge in object keys.
func (f *forge) host() string { return strings.TrimPrefix(f.base, "http://") }

func (f *forge) repoDir(owner, name string) string { return filepath.Join(f.root, owner, name+".git") }
func (f *forge) lfsDir(owner, name string) string  { return filepath.Join(f.lfsRoot, owner, name) }

// addRepo makes a repository visible to the API. Its git directory is the fixtures' to create.
func (f *forge) addRepo(owner, name string, sizeKB int64, meta repoMeta) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[owner+"/"+name] = &forgeRepo{owner: owner, name: name, id: int64(len(f.repos) + 1000), sizeKB: sizeKB, meta: meta}
}

// addInstallation makes an App installation that can see slugs, in that order.
func (f *forge) addInstallation(id int64, slugs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs[id] = &installation{id: id, repos: slugs}
}

// setRateLimit gives an installation a budget of limit REST requests per window.
func (f *forge) setRateLimit(id int64, limit int, window time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in := f.installs[id]
	in.limit, in.window, in.used, in.reset = limit, window, 0, time.Time{}
}

func (f *forge) stats() forgeStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st.clone()
}

func (f *forge) count(what string) {
	f.mu.Lock()
	f.st.Requests[what]++
	f.mu.Unlock()
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)

// fetchHold stops the next clone of one repository until it is released: the request has
// arrived and its pack is not being sent.
type fetchHold struct {
	slug    string
	reached chan struct{}
	release chan struct{}
	fired   bool
	once    sync.Once
}

func (f *forge) holdFetch(slug string) *fetchHold {
	hd := &fetchHold{slug: slug, reached: make(chan struct{}), release: make(chan struct{})}
	f.mu.Lock()
	f.holds = append(f.holds, hd)
	f.mu.Unlock()
	return hd
}

func (hd *fetchHold) Release() { hd.once.Do(func() { close(hd.release) }) }

// ---- REST ----

// api authenticates a REST request with an installation token, spends from the installation's
// budget, and writes the handler's answer as JSON.
func (f *forge) api(name string, fn func(r *http.Request, in *installation) (int, any, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.count("api:" + name)
		tok := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "token "), "Bearer "))
		in := f.tokenInstall(tok)
		if in == nil {
			f.mu.Lock()
			f.st.Unauthorized++
			f.mu.Unlock()
			writeAPI(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
			return
		}
		f.mu.Lock()
		limit, remaining, reset, ok := in.spend(time.Now())
		if !ok {
			f.st.RateLimited++
		} else if remaining == 0 {
			f.st.BudgetSpent++
		}
		f.mu.Unlock()
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		w.Header().Set("X-RateLimit-Used", strconv.Itoa(limit-remaining))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(ceilUnix(reset), 10))
		w.Header().Set("X-RateLimit-Resource", "core")
		if !ok {
			writeAPI(w, http.StatusForbidden, map[string]string{
				"message":           fmt.Sprintf("API rate limit exceeded for installation ID %d.", in.id),
				"documentation_url": "https://docs.github.com/rest/overview/resources-in-the-rest-api#rate-limiting",
			})
			return
		}
		status, body, link := fn(r, in)
		if link != "" {
			w.Header().Set("Link", link)
		}
		writeAPI(w, status, body)
	}
}

func ceilUnix(t time.Time) int64 {
	s := t.Unix()
	if t.Nanosecond() > 0 {
		s++
	}
	return s
}

// spend takes one request from the budget. The window starts at the first request after the
// last one ended, as GitHub's does.
func (in *installation) spend(now time.Time) (limit, remaining int, reset time.Time, ok bool) {
	limit = in.limit
	if limit == 0 {
		limit = 100_000_000
	}
	window := in.window
	if window == 0 {
		window = time.Hour
	}
	if in.reset.IsZero() || !now.Before(in.reset) {
		in.used, in.reset = 0, now.Add(window)
	}
	if in.used >= limit {
		return limit, 0, in.reset, false
	}
	in.used++
	return limit, limit - in.used, in.reset, true
}

func writeAPI(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *forge) tokenInstall(tok string) *installation {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.tokens[tok]
	if !ok || time.Now().After(g.expires) {
		return nil
	}
	return f.installs[g.install]
}

// accessToken mints an installation token for a request signed with the App's key, as
// POST /app/installations/{id}/access_tokens does.
func (f *forge) accessToken(w http.ResponseWriter, r *http.Request) {
	f.count("api:app/access_tokens")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPI(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	if err := f.verifyAppJWT(r.Header.Get("Authorization")); err != nil {
		f.mu.Lock()
		f.st.Unauthorized++
		f.mu.Unlock()
		writeAPI(w, http.StatusUnauthorized, map[string]string{"message": "A JSON web token could not be decoded: " + err.Error()})
		return
	}
	tok := "ghs_" + randomHex(18)
	expires := time.Now().Add(f.tokenTTL)
	f.mu.Lock()
	_, ok := f.installs[id]
	if ok {
		f.tokens[tok] = grant{install: id, expires: expires}
		f.st.TokensMinted++
	}
	f.mu.Unlock()
	if !ok {
		writeAPI(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	writeAPI(w, http.StatusCreated, map[string]any{
		"token":                tok,
		"expires_at":           expires.UTC().Format(time.RFC3339),
		"repository_selection": "selected",
		"permissions":          map[string]string{"contents": "read", "metadata": "read", "issues": "read", "pull_requests": "read"},
	})
}

// verifyAppJWT checks an RS256 token signed with the App's key and issued by its id.
func (f *forge) verifyAppJWT(header string) error {
	tok, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return errors.New("no bearer token")
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return errors.New("not a JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(f.appPub, crypto.SHA256, sum[:], sig); err != nil {
		return err
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var claims struct {
		Iss json.RawMessage `json:"iss"`
		Exp int64           `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return err
	}
	if iss := strings.Trim(string(claims.Iss), `"`); iss != strconv.FormatInt(f.appID, 10) {
		return fmt.Errorf("issuer %s is not this App", iss)
	}
	if time.Unix(claims.Exp, 0).Before(time.Now()) {
		return errors.New("expired")
	}
	return nil
}

func (f *forge) installationRepos(r *http.Request, in *installation) (int, any, string) {
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage <= 0 {
		perPage = 30
	}
	perPage = min(perPage, 100)
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	total := len(in.repos)
	last := max((total+perPage-1)/perPage, 1)
	var list []map[string]any
	for i := (page - 1) * perPage; i < min(page*perPage, total); i++ {
		list = append(list, f.repoJSON(r.Host, f.repos[in.repos[i]]))
	}
	if list == nil {
		list = []map[string]any{}
	}
	var link string
	if page < last {
		u := url.URL{Scheme: "http", Host: r.Host, Path: r.URL.Path}
		at := func(p int) string {
			q := url.Values{"per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(p)}}
			u.RawQuery = q.Encode()
			return u.String()
		}
		link = fmt.Sprintf(`<%s>; rel="next", <%s>; rel="last"`, at(page+1), at(last))
	}
	return http.StatusOK, map[string]any{"total_count": total, "repository_selection": "selected", "repositories": list}, link
}

// visible is the repository a request names, if the installation can see it.
func (f *forge) visible(r *http.Request, in *installation) *forgeRepo {
	slug := r.PathValue("owner") + "/" + r.PathValue("repo")
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range in.repos {
		if s == slug {
			return f.repos[slug]
		}
	}
	return nil
}

func (f *forge) getRepo(r *http.Request, in *installation) (int, any, string) {
	rp := f.visible(r, in)
	if rp == nil {
		return http.StatusNotFound, map[string]string{"message": "Not Found"}, ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return http.StatusOK, f.repoJSON(r.Host, rp), ""
}

func (f *forge) listMeta(pick func(repoMeta) []map[string]any) func(*http.Request, *installation) (int, any, string) {
	return func(r *http.Request, in *installation) (int, any, string) {
		rp := f.visible(r, in)
		if rp == nil {
			return http.StatusNotFound, map[string]string{"message": "Not Found"}, ""
		}
		list := pick(rp.meta)
		if list == nil {
			list = []map[string]any{}
		}
		return http.StatusOK, list, ""
	}
}

func (f *forge) repoJSON(host string, rp *forgeRepo) map[string]any {
	slug := rp.owner + "/" + rp.name
	return map[string]any{
		"id":             rp.id,
		"node_id":        fmt.Sprintf("R_%d", rp.id),
		"name":           rp.name,
		"full_name":      slug,
		"private":        true,
		"visibility":     "private",
		"owner":          map[string]any{"login": rp.owner, "id": 77, "type": "Organization"},
		"html_url":       "http://" + host + "/" + slug,
		"clone_url":      "http://" + host + "/" + slug + ".git",
		"default_branch": "main",
		"archived":       false,
		"size":           rp.sizeKB,
		"created_at":     "2026-01-01T00:00:00Z",
		"updated_at":     "2026-01-01T00:00:00Z",
		"pushed_at":      "2026-01-01T00:00:00Z",
	}
}

// ---- git ----

func (f *forge) gitSlug(r *http.Request) (owner, name string, ok bool) {
	owner = r.PathValue("owner")
	name, ok = strings.CutSuffix(r.PathValue("repo"), ".git")
	return owner, name, ok && nameRE.MatchString(owner) && nameRE.MatchString(name)
}

// gitAuth checks the installation token git sends as its password.
func (f *forge) gitAuth(r *http.Request, slug string) bool {
	_, tok, ok := r.BasicAuth()
	if ok {
		if in := f.tokenInstall(tok); in != nil {
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, s := range in.repos {
				if s == slug {
					return true
				}
			}
		}
	}
	f.mu.Lock()
	f.st.Unauthorized++
	f.mu.Unlock()
	return false
}

func (f *forge) git(w http.ResponseWriter, r *http.Request) {
	owner, name, ok := f.gitSlug(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	slug := owner + "/" + name
	kind := "git:info-refs"
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(io.LimitReader(r.Body, 256<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Protocol v2 posts its ref listing here too. A clone's fetch is what a hold is for; an
		// ls-refs request is small, so git never compresses it, and it names its command.
		kind = "git:fetch"
		if r.Header.Get("Content-Encoding") == "" && bytes.Contains(body, []byte("command=ls-refs")) {
			kind = "git:ls-refs"
		}
		// git sends a large negotiation chunked, and net/http/cgi refuses a chunked body. It is
		// read whole above, so it goes on with a length.
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.TransferEncoding = nil
	}
	f.count(kind)
	if !f.gitAuth(r, slug) {
		w.Header().Set("WWW-Authenticate", `Basic realm="fakeforge"`)
		http.Error(w, "Bad credentials", http.StatusUnauthorized)
		return
	}
	if kind == "git:fetch" && !f.waitHold(r, slug) {
		return
	}
	cw := &countingWriter{ResponseWriter: w}
	f.cgi.ServeHTTP(cw, r)
	f.mu.Lock()
	f.st.UploadPackBytes += cw.n
	f.mu.Unlock()
}

// waitHold parks a clone that a hold is waiting for, and reports whether its client is still
// there once the hold is released.
func (f *forge) waitHold(r *http.Request, slug string) bool {
	f.mu.Lock()
	var hd *fetchHold
	for _, x := range f.holds {
		if x.slug == slug && !x.fired {
			x.fired = true
			hd = x
			break
		}
	}
	f.mu.Unlock()
	if hd == nil {
		return true
	}
	close(hd.reached)
	select {
	case <-hd.release:
		return r.Context().Err() == nil
	case <-r.Context().Done():
		return false
	}
}

type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n += int64(n)
	return n, err
}

// ---- LFS ----

var oidRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (f *forge) lfsBatch(w http.ResponseWriter, r *http.Request) {
	f.count("lfs:batch")
	owner, name, ok := f.gitSlug(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !f.gitAuth(r, owner+"/"+name) {
		w.Header().Set("WWW-Authenticate", `Basic realm="fakeforge"`)
		writeAPI(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	var req struct {
		Operation string `json:"operation"`
		Objects   []struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"objects"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
		writeAPI(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
		return
	}
	if req.Operation != "download" {
		writeAPI(w, http.StatusForbidden, map[string]string{"message": "this forge is read-only"})
		return
	}
	expires := time.Now().Add(f.linkTTL)
	objects := make([]map[string]any, 0, len(req.Objects))
	for _, o := range req.Objects {
		entry := map[string]any{"oid": o.OID, "size": o.Size}
		if !oidRE.MatchString(o.OID) {
			entry["error"] = map[string]any{"code": 422, "message": "invalid oid"}
		} else if _, err := os.Stat(filepath.Join(f.lfsDir(owner, name), o.OID)); err != nil {
			entry["error"] = map[string]any{"code": 404, "message": "Object does not exist"}
		} else {
			exp := strconv.FormatInt(expires.Unix(), 10)
			q := url.Values{"exp": {exp}, "sig": {f.linkSig(owner, name, o.OID, exp)}}
			entry["authenticated"] = true
			entry["actions"] = map[string]any{"download": map[string]any{
				"href":       "http://" + r.Host + "/_lfs/" + owner + "/" + name + "/objects/" + o.OID + "?" + q.Encode(),
				"expires_in": int(f.linkTTL.Seconds()),
			}}
		}
		objects = append(objects, entry)
	}
	w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
	_ = json.NewEncoder(w).Encode(map[string]any{"transfer": "basic", "objects": objects})
}

func (f *forge) linkSig(owner, name, oid, exp string) string {
	m := hmac.New(sha256.New, f.linkKey)
	m.Write([]byte(owner + "/" + name + "/" + oid + "/" + exp))
	return hex.EncodeToString(m.Sum(nil))
}

// lfsObject serves one object to a link the batch API handed out, until the link expires.
func (f *forge) lfsObject(w http.ResponseWriter, r *http.Request) {
	f.count("lfs:download")
	owner, repo, oid := r.PathValue("owner"), r.PathValue("repo"), r.PathValue("oid")
	exp := r.URL.Query().Get("exp")
	if !nameRE.MatchString(owner) || !nameRE.MatchString(repo) || !oidRE.MatchString(oid) ||
		!hmac.Equal([]byte(r.URL.Query().Get("sig")), []byte(f.linkSig(owner, repo, oid, exp))) {
		http.Error(w, "bad link", http.StatusForbidden)
		return
	}
	if at, err := strconv.ParseInt(exp, 10, 64); err != nil || time.Now().After(time.Unix(at, 0)) {
		f.count("lfs:expired-link")
		http.Error(w, "link expired", http.StatusForbidden)
		return
	}
	fh, err := os.Open(filepath.Join(f.lfsDir(owner, repo), oid))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = fh.Close() }()
	info, err := fh.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, oid, info.ModTime(), fh)
	f.mu.Lock()
	f.st.LFSBytes += cw.n
	f.mu.Unlock()
}
