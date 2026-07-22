// Package github implements the read-only Source for GitHub.com (and, by base URL,
// GitHub Enterprise Server). Auth is a GitHub App installation token, short-lived,
// least-privilege, and App-compatible. Metadata uses per-resource REST endpoints, not
// the Migrations API.
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
	"github.com/google/go-github/v89/github"

	"gitdr.io/gitdr/internal/source"
)

// Options configures the GitHub source.
type Options struct {
	BaseURL        string // empty = github.com; GHES: https://host/api/v3
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
}

// Source is a read-only GitHub backend.
type Source struct {
	client    *github.Client
	transport *ghinstallation.Transport
	host      string
	logger    *slog.Logger
}

var (
	_ source.Source    = (*Source)(nil)
	_ source.GitAuther = (*Source)(nil)
)

// New builds a GitHub source authenticated as an App installation.
func New(opts Options, logger *slog.Logger) (*Source, error) {
	if opts.AppID == 0 || opts.InstallationID == 0 {
		return nil, errors.New("github: appID and installationID are required")
	}
	if len(opts.PrivateKeyPEM) == 0 {
		return nil, errors.New("github: app private key is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	tr, err := ghinstallation.New(http.DefaultTransport, opts.AppID, opts.InstallationID, opts.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("github: app transport: %w", err)
	}
	httpClient := &http.Client{Transport: tr, Timeout: 60 * time.Second}

	clientOpts := []github.ClientOptionsFunc{
		github.WithHTTPClient(httpClient),
		github.WithUserAgent("gitdr"),
	}
	host := "github.com"
	if opts.BaseURL != "" {
		clientOpts = append(clientOpts, github.WithEnterpriseURLs(opts.BaseURL, opts.BaseURL))
		tr.BaseURL = strings.TrimRight(opts.BaseURL, "/") // token endpoint must hit GHES too
		host = hostFromURL(opts.BaseURL)
	}
	client, err := github.NewClient(clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("github: client: %w", err)
	}
	return &Source{client: client, transport: tr, host: host, logger: logger}, nil
}

// ListRepos returns repositories accessible to the installation, filtered.
func (s *Source) ListRepos(ctx context.Context, f source.Filter) ([]source.Repo, error) {
	opt := &github.ListOptions{PerPage: 100}
	var out []source.Repo
	for {
		list, resp, err := s.client.Apps.ListRepos(ctx, opt)
		if err != nil {
			return nil, fmt.Errorf("github: list installation repos: %w", err)
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

// GitAuthHeader mints an installation token and returns it as a Basic auth header
// (username x-access-token). Injected into git via env so it never hits argv.
func (s *Source) GitAuthHeader(ctx context.Context) (string, error) {
	tok, err := s.transport.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("github: installation token: %w", err)
	}
	cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
	return "Authorization: Basic " + cred, nil
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
