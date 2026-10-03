// Package github implements the read-only Source for GitHub.com (and, by base URL,
// GitHub Enterprise Server). Auth is a GitHub App installation token, short-lived,
// least-privilege, and App-compatible: minted here from the App's private key, or read from a
// file somebody else mints it into (tokenfile.go). Metadata uses per-resource REST endpoints,
// not the Migrations API.
package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	ghinstallation "github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v90/github"

	"gitdr.io/gitdr/internal/redact"
	"gitdr.io/gitdr/internal/source"
)

// Options configures the GitHub source: TokenPath, or AppID, InstallationID and PrivateKeyPEM.
type Options struct {
	BaseURL        string // empty = github.com; GHES: https://host/api/v3
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
	// TokenPath names a file holding an installation token. With it the source holds no key
	// and mints nothing; it reads the file before every API request and every git command.
	TokenPath string
}

// Source is a read-only GitHub backend.
type Source struct {
	client    *github.Client
	transport *ghinstallation.Transport // App mode: mints and renews the installation token
	tokenFile *tokenFile                // token-file mode: somebody else does
	host      string
	logger    *slog.Logger
	// The clock a wait for a rate limit is measured and spent on. Fields, so a test can spend an
	// hour's wait without serving it.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

var (
	_ source.Source    = (*Source)(nil)
	_ source.GitAuther = (*Source)(nil)
)

// New builds a GitHub source authenticated as an App installation, with a token it mints from
// the App's key or one it reads from Options.TokenPath.
func New(opts Options, logger *slog.Logger) (*Source, error) {
	return newSource(opts, logger, http.DefaultTransport)
}

// newSource is New over a given transport, which is how the tests reach a TLS server whose
// certificate the default transport would refuse.
func newSource(opts Options, logger *slog.Logger, base http.RoundTripper) (*Source, error) {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Source{host: "github.com", logger: logger, now: time.Now, sleep: sleepContext}
	apiHost := "api.github.com"
	if opts.BaseURL != "" {
		s.host = hostFromURL(opts.BaseURL)
		apiHost = ""
		if u, err := url.Parse(opts.BaseURL); err == nil {
			apiHost = u.Host // empty when unparseable, and then no request carries the token
		}
	}

	var rt http.RoundTripper
	if opts.TokenPath != "" {
		if len(opts.PrivateKeyPEM) > 0 {
			return nil, errors.New("github: both a token file and an App private key were given; set exactly one")
		}
		tf := &tokenFile{path: opts.TokenPath}
		// Read once now, so a file that is missing or malformed fails the run before any
		// work starts. The value is not kept; every request reads the file again.
		if _, err := tf.read(); err != nil {
			return nil, err
		}
		s.tokenFile = tf
		rt = &tokenTransport{base: base, file: *tf, host: apiHost}
	} else {
		if opts.AppID == 0 || opts.InstallationID == 0 {
			return nil, errors.New("github: appID and installationID are required")
		}
		if len(opts.PrivateKeyPEM) == 0 {
			return nil, errors.New("github: app private key is required")
		}
		tr, err := ghinstallation.New(base, opts.AppID, opts.InstallationID, opts.PrivateKeyPEM)
		if err != nil {
			return nil, fmt.Errorf("github: app transport: %w", err)
		}
		if opts.BaseURL != "" {
			tr.BaseURL = strings.TrimRight(opts.BaseURL, "/") // token endpoint must hit GHES too
		}
		s.transport = tr
		rt = tr
	}
	httpClient := &http.Client{Transport: rt, Timeout: 60 * time.Second}

	clientOpts := []github.ClientOptionsFunc{
		github.WithHTTPClient(httpClient),
		github.WithUserAgent("gitdr"),
	}
	if opts.BaseURL != "" {
		clientOpts = append(clientOpts, github.WithEnterpriseURLs(opts.BaseURL, opts.BaseURL))
	}
	client, err := github.NewClient(clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("github: client: %w", err)
	}
	s.client = client
	return s, nil
}

// UsesTokenFile reports whether the token comes from a token file rather than the App key.
func (s *Source) UsesTokenFile() bool { return s.tokenFile != nil }

// CheckToken makes one API request with the token in the token file, for a single page of one
// repository the installation can see. It is how doctor tests a token gitdr did not mint, and
// so cannot test by minting.
//
// GET /installation/repositories accepts nothing but an installation token, so a personal
// access token fails here as it would fail the backup.
func (s *Source) CheckToken(ctx context.Context) error {
	if s.tokenFile == nil {
		return errors.New("github: this source mints its own token; there is no token file to check")
	}
	if _, _, err := s.client.Apps.ListRepos(ctx, &github.ListOptions{PerPage: 1}); err != nil {
		return fmt.Errorf("github: checking the token from source.github.tokenPath: %w", err)
	}
	return nil
}

// ListRepos returns repositories accessible to the installation, filtered. Each page waits out a
// rate limit and retries a 5xx; see call.
func (s *Source) ListRepos(ctx context.Context, f source.Filter) ([]source.Repo, error) {
	opt := &github.ListOptions{PerPage: 100}
	var out []source.Repo
	for {
		var list *github.ListRepositories
		var resp *github.Response
		err := s.call(ctx, func(ctx context.Context) error {
			var err error
			list, resp, err = s.client.Apps.ListRepos(ctx, opt)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("github: list installation repos: %w", err)
		}
		// go-github leaves the list nil on a 200 with an empty body. That is not "no
		// repositories", it is no answer, and reading it as none would back up nothing and succeed.
		if list == nil || resp == nil {
			return nil, errors.New("github: list installation repos: GitHub answered with no list")
		}
		for _, r := range list.Repositories {
			repo := toRepo(s.host, r)
			if keep(repo, f) {
				out = append(out, repo)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// CloneURL returns the HTTPS clone URL (no embedded credentials).
func (s *Source) CloneURL(_ context.Context, r source.Repo) (string, error) {
	if r.CloneURL != "" {
		return r.CloneURL, nil
	}
	return fmt.Sprintf("https://%s/%s/%s.git", s.host, r.Owner, r.Name), nil
}

// GitAuthHeader returns the installation token as a Basic auth header (username
// x-access-token), injected into git via env so it never hits argv.
//
// It is asked for once per git command. With a token file it reads the file each time. In App
// mode the transport keeps the token it minted and mints a new one within a minute of its
// expiry, so a run longer than the token's hour keeps working.
func (s *Source) GitAuthHeader(ctx context.Context) (string, error) {
	if s.tokenFile != nil {
		tok, err := s.tokenFile.read()
		if err != nil {
			return "", err
		}
		return basicAuthHeader(tok), nil
	}
	tok, err := s.transport.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("github: installation token: %w", err)
	}
	return basicAuthHeader(redact.Secret(tok)), nil
}

// basicAuthHeader is the header git sends: GitHub takes an installation token as the password
// of the user x-access-token.
func basicAuthHeader(tok redact.Secret) string {
	cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok.Reveal()))
	return "Authorization: Basic " + cred
}

func toRepo(host string, r *github.Repository) source.Repo {
	return source.Repo{
		Host:          host,
		Owner:         r.GetOwner().GetLogin(),
		Name:          r.GetName(),
		CloneURL:      r.GetCloneURL(),
		DefaultBranch: r.GetDefaultBranch(),
		Archived:      r.GetArchived(),
		SizeKB:        int64(r.GetSize()),
	}
}

// keep applies the include/exclude filter. Exclude wins; empty Include keeps all.
func keep(r source.Repo, f source.Filter) bool {
	for _, ex := range f.Exclude {
		if matches(ex, r) {
			return false
		}
	}
	if len(f.Include) == 0 {
		return true
	}
	for _, in := range f.Include {
		if matches(in, r) {
			return true
		}
	}
	return false
}

// matches a repo against a pattern: glob (e.g. "org/*") on slug or name, else exact
// case-insensitive.
func matches(pat string, r source.Repo) bool {
	if ok, _ := path.Match(pat, r.Slug()); ok {
		return true
	}
	if ok, _ := path.Match(pat, r.Name); ok {
		return true
	}
	return strings.EqualFold(pat, r.Slug()) || strings.EqualFold(pat, r.Name)
}

func hostFromURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return "github.com"
}
