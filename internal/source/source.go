// Package source defines the read-only Source interface implemented by every VCS
// backend (GitHub, GHES, GitLab, ...). It only enumerates repos and yields what's
// needed to clone and dump them, no method mutates the upstream VCS.
package source

import (
	"context"
	"errors"
)

// ErrTransient marks a failure that is likely to pass: a rate limit that could not be waited out,
// or a server error that outlasted its retries.
//
// The pipeline fetches metadata before it writes anything, and stops a repository whose metadata
// failed this way with nothing stored, because the next run will probably get through. Any other
// failure, a missing permission say, would fail every run the same way, so the repository's code
// is stored regardless and only then is the repository failed. A source that marks nothing gets
// the second behaviour for every failure, which is the safe one.
var ErrTransient = errors.New("transient failure")

// Transient marks err as ErrTransient, keeping its message. It returns nil for nil.
func Transient(err error) error {
	if err == nil {
		return nil
	}
	return transientError{err}
}

type transientError struct{ err error }

func (e transientError) Error() string   { return e.err.Error() }
func (e transientError) Unwrap() []error { return []error{e.err, ErrTransient} }

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
//
// It is called once per git command, immediately before the command starts, so it must be
// cheap: a run over a large organisation calls it thousands of times. Asking each time is what
// lets a credential that changes during a run, a token file replaced or an App token renewed,
// reach the next command instead of the next run.
type GitAuther interface {
	// GitAuthHeader returns a full HTTP header line, e.g. "Authorization: Basic ...".
	GitAuthHeader(ctx context.Context) (string, error)
}
