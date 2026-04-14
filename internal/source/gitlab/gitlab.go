// Package gitlab implements the read-only Source for GitLab.com and self-managed
// GitLab. Auth is a read-scoped access token (project, group, or personal). It has no
// mutating operations.
package gitlab

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"gitdr.io/gitdr/internal/source"
)

// Options configures the GitLab source.
type Options struct {
	BaseURL string // empty = gitlab.com; self-managed: https://gitlab.example.com
	Token   string // read-scoped access token
}

// Source is a read-only GitLab backend.
type Source struct {
	client *gitlab.Client
	token  string
	host   string
	logger *slog.Logger
}

var (
	_ source.Source    = (*Source)(nil)
	_ source.GitAuther = (*Source)(nil)
)

// New builds a GitLab source from a read-scoped access token.
func New(opts Options, logger *slog.Logger) (*Source, error) {
	if strings.TrimSpace(opts.Token) == "" {
		return nil, errors.New("gitlab: access token is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	var clientOpts []gitlab.ClientOptionFunc
	host := "gitlab.com"
	if opts.BaseURL != "" {
		clientOpts = append(clientOpts, gitlab.WithBaseURL(opts.BaseURL))
		host = hostFromURL(opts.BaseURL)
	}
	client, err := gitlab.NewClient(opts.Token, clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("gitlab: new client: %w", err)
	}
	return &Source{client: client, token: opts.Token, host: host, logger: logger}, nil
}

// ListRepos returns projects the token can access, filtered.
func (s *Source) ListRepos(ctx context.Context, f source.Filter) ([]source.Repo, error) {
	opt := &gitlab.ListProjectsOptions{
		ListOptions: gitlab.ListOptions{PerPage: 100},
		Membership:  gitlab.Ptr(true),
	}
	var out []source.Repo
	for {
		projects, resp, err := s.client.Projects.ListProjects(opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("gitlab: list projects: %w", err)
		}
		for _, p := range projects {
			repo := s.toRepo(p)
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

// GitAuthHeader returns the token as Basic auth (username oauth2) for git over HTTPS,
// injected into git via env so it never hits argv.
func (s *Source) GitAuthHeader(_ context.Context) (string, error) {
	cred := base64.StdEncoding.EncodeToString([]byte("oauth2:" + s.token))
	return "Authorization: Basic " + cred, nil
}

func (s *Source) toRepo(p *gitlab.Project) source.Repo {
	owner := ""
	if p.Namespace != nil {
		owner = p.Namespace.FullPath
	}
	return source.Repo{
		Host:          s.host,
		Owner:         owner,
		Name:          p.Path,
		CloneURL:      p.HTTPURLToRepo,
		DefaultBranch: p.DefaultBranch,
		Archived:      p.Archived,
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

// matches a repo against a glob (e.g. "group/*") on slug or name, else exact (ci).
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
	return "gitlab.com"
}
