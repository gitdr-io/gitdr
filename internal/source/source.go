// Package source defines the read-only Source interface implemented by every VCS
// backend (GitHub, GHES, GitLab, ...). It only enumerates repos and yields what's
// needed to clone and dump them, no method mutates the upstream VCS.
package source

import "context"

// Repo identifies a single repository discovered on a Source, plus the minimal
// attributes the pipeline needs to back it up.
type Repo struct {
	Host          string `json:"host"`          // VCS host, e.g. "github.com"
	Owner         string `json:"owner"`         // org or user that owns the repo
	Name          string `json:"name"`          // repository name (no owner prefix)
	CloneURL      string `json:"cloneUrl"`      // HTTPS clone URL; credentials are applied at clone time, never embedded here
	DefaultBranch string `json:"defaultBranch"` // default branch name, if known
	Archived      bool   `json:"archived"`      // upstream archived flag
	SizeKB        int64  `json:"sizeKb"`        // approximate size in KiB, if reported
}

// Slug returns the "owner/name" identifier for the repo.
func (r Repo) Slug() string { return r.Owner + "/" + r.Name }

// Filter narrows which repositories a backup run targets. An empty Filter selects
// everything the credential can see. Include/Exclude are matched by the Source
// implementation (typically against "owner/name"); Exclude wins over Include.
type Filter struct {
	Include []string // if non-empty, only repos matching one of these are kept
	Exclude []string // repos matching any of these are dropped
}

// Source is the read-only interface implemented by every VCS backend.
//
// Invariant: a Source exposes no mutating operation. It can only read.
type Source interface {
	// ListRepos enumerates repositories visible to the configured identity,
	// applying the include/exclude filter.
	ListRepos(ctx context.Context, f Filter) ([]Repo, error)

	// CloneURL returns the URL to clone r over HTTPS. Authentication is supplied
	// out of band at clone time (an injected Authorization header); the returned
	// URL must not embed credentials.
	CloneURL(ctx context.Context, r Repo) (string, error)

	// FetchMetadata returns repository metadata as JSON bytes for archival. In the
	// walking skeleton this is a minimal repo descriptor; the full per-resource dump
	// (issues, PRs, releases, ...) is added later (M5).
	FetchMetadata(ctx context.Context, r Repo) ([]byte, error)
}

// GitAuther is an optional interface for Sources that authenticate git clones over
// HTTPS. The pipeline injects the returned header into git via env, never argv, so the
// token stays out of process listings. Sources without auth (e.g. local fixtures)
// simply don't implement it.
type GitAuther interface {
	// GitAuthHeader returns a full HTTP header line, e.g. "Authorization: Basic ...".
	GitAuthHeader(ctx context.Context) (string, error)
}
