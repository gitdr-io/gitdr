//go:build scale

package scale

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v90/github"

	"gitdr.io/gitdr/internal/gitexec"
	"gitdr.io/gitdr/internal/source"
	ghsrc "gitdr.io/gitdr/internal/source/github"
)

// The fake forge, checked against the engine's own GitHub client and git wrapper. A scenario that
// reports a rate limit or an expired link is only as good as the fake that produced it. Needs no
// Docker: `go test -tags scale -run Forge ./scale`.
func TestForgeAnswersLikeGitHub(t *testing.T) {
	f := h.needForge(t)
	const install = 90
	o := &org{owner: "self-check", install: install}
	for i := range 250 {
		o.names = append(o.names, fmt.Sprintf("listed-%03d", i))
		f.addRepo(o.owner, o.names[i], 1, plainMeta())
	}
	if err := initBare(f.repoDir(o.owner, o.names[0])); err != nil {
		t.Fatal(err)
	}
	if err := fastImport(f.repoDir(o.owner, o.names[0]), bytes.NewReader(smallStream(o.names[0], 1))); err != nil {
		t.Fatal(err)
	}
	if err := h.seedLFS(f, o.owner, "lfs-check", 2, 1024); err != nil {
		t.Fatal(err)
	}
	o.names = append(o.names, "lfs-check")
	f.addInstallation(install, slugs(o))
	ctx := context.Background()

	src, err := ghsrc.New(ghsrc.Options{BaseURL: f.apiURL(), AppID: f.appID, InstallationID: install, PrivateKeyPEM: f.appKeyPEM}, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("the installation lists every repository, in pages", func(t *testing.T) {
		before := f.stats().Requests["api:installation/repositories"]
		repos, err := src.ListRepos(ctx, source.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(repos) != len(o.names) || repos[0].Name != o.names[0] || repos[len(repos)-1].Name != "lfs-check" {
			t.Fatalf("listed %d repositories, want %d in order", len(repos), len(o.names))
		}
		if pages := f.stats().Requests["api:installation/repositories"] - before; pages != 3 {
			t.Errorf("%d pages, want 3 of up to 100", pages)
		}
		if !strings.HasSuffix(repos[0].CloneURL, "/self-check/listed-000.git") {
			t.Errorf("clone url %s", repos[0].CloneURL)
		}
	})

	t.Run("metadata is fetched through every list endpoint", func(t *testing.T) {
		meta, err := src.FetchMetadata(ctx, source.Repo{Host: f.host(), Owner: o.owner, Name: o.names[0]})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(meta, []byte(`"enhancement"`)) {
			t.Errorf("no labels in %s", meta)
		}
	})

	t.Run("git needs the installation token", func(t *testing.T) {
		g := gitexec.New(nil)
		cloneURL := f.base + "/" + o.owner + "/" + o.names[0] + ".git"
		auth, err := src.GitAuthHeader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		refs, err := g.LsRemote(ctx, cloneURL, gitexec.Options{AuthHeader: auth})
		if err != nil || refs["refs/heads/main"] == "" {
			t.Fatalf("ls-remote with the token: %v %v", refs, err)
		}
		if _, err := g.LsRemote(ctx, cloneURL, gitexec.Options{}); err == nil {
			t.Error("ls-remote without a token was answered")
		}
		f.mu.Lock()
		f.tokens["ghs_expired"] = grant{install: install, expires: time.Now().Add(-time.Second)}
		f.mu.Unlock()
		expired := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_expired"))
		if _, err := g.LsRemote(ctx, cloneURL, gitexec.Options{AuthHeader: expired}); err == nil {
			t.Error("ls-remote with an expired token was answered")
		}
	})

	t.Run("a spent budget answers 403 with the reset, as GitHub does", func(t *testing.T) {
		f.setRateLimit(install, 3, time.Hour)
		defer f.setRateLimit(install, 0, 0)
		_, err := src.FetchMetadata(ctx, source.Repo{Host: f.host(), Owner: o.owner, Name: o.names[0]})
		var rle *github.RateLimitError
		if !errors.As(err, &rle) {
			t.Fatalf("err = %v, want a *github.RateLimitError", err)
		}
		if until := time.Until(rle.Rate.Reset.Time); until < 50*time.Minute || until > time.Hour+time.Minute {
			t.Errorf("reset in %s, want about an hour", until)
		}
	})

	t.Run("an LFS link works until it expires", func(t *testing.T) {
		auth, err := src.GitAuthHeader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// The second object is on the pull request only; any object will do for a link.
		var oid string
		for _, e := range mustReadDir(t, f.lfsDir(o.owner, "lfs-check")) {
			oid = e
		}
		body := fmt.Sprintf(`{"operation":"download","transfers":["basic"],"objects":[{"oid":%q,"size":1024}]}`, oid)
		req, _ := http.NewRequest(http.MethodPost, f.base+"/self-check/lfs-check.git/info/lfs/objects/batch", strings.NewReader(body))
		req.Header.Set("Authorization", strings.TrimPrefix(auth, "Authorization: "))
		req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var batch struct {
			Objects []struct {
				Actions struct {
					Download struct {
						Href string `json:"href"`
					} `json:"download"`
				} `json:"actions"`
			} `json:"objects"`
		}
		err = json.NewDecoder(resp.Body).Decode(&batch)
		_ = resp.Body.Close()
		if err != nil || len(batch.Objects) != 1 || batch.Objects[0].Actions.Download.Href == "" {
			t.Fatalf("batch answered %d: %+v %v", resp.StatusCode, batch, err)
		}
		href := batch.Objects[0].Actions.Download.Href
		if n, status := fetch(t, href); status != http.StatusOK || n != 1024 {
			t.Errorf("download: %d, %d bytes", status, n)
		}
		u, _ := url.Parse(href)
		past := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
		u.RawQuery = url.Values{"exp": {past}, "sig": {f.linkSig(o.owner, "lfs-check", oid, past)}}.Encode()
		if _, status := fetch(t, u.String()); status != http.StatusForbidden {
			t.Errorf("an expired link answered %d, want 403", status)
		}
	})
}

func fetch(t *testing.T, u string) (int64, int) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	n, _ := io.Copy(io.Discard, resp.Body)
	return n, resp.StatusCode
}

func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := readDirNames(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
