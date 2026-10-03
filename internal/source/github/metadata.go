package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/go-github/v90/github"

	"gitdr.io/gitdr/internal/source"
)

// metaSchema versions the metadata document. It is audit/reference data, not a
// faithful, restorable snapshot (see SPEC §7), the API cannot recreate original
// numbers, authors, or timestamps.
const metaSchema = "gitdr.meta/v1"

// FetchMetadata dumps per-resource metadata as gitdr.meta/v1 JSON using App-compatible
// per-resource REST endpoints (never the Migrations API). Every request waits out a rate limit
// and retries a 5xx; see call.
func (s *Source) FetchMetadata(ctx context.Context, r source.Repo) ([]byte, error) {
	owner, name := r.Owner, r.Name
	doc := map[string]any{
		"schema":    metaSchema,
		"host":      r.Host,
		"owner":     owner,
		"name":      name,
		"fetchedAt": time.Now().UTC().Format(time.RFC3339),
	}

	var repo *github.Repository
	err := s.call(ctx, func(ctx context.Context) error {
		var err error
		repo, _, err = s.client.Repositories.Get(ctx, owner, name)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("github: get repo %s: %w", r.Slug(), err)
	}
	doc["repo"] = repo

	// A section GitHub answers with 404 or 410 is a feature turned off for this repository. With
	// pull requests off it answers 404 for them, and with issues off as well, 404 for the issue
	// comments. Such a section is stored empty and named under "unavailable" with GitHub's answer,
	// so it is never mistaken for a section that was read and held nothing. Up to v0.1.20 it failed
	// the repository on every run. Any other refusal still fails it.
	unavailable := map[string]string{}

	if err := section(ctx, s, doc, unavailable, "labels", func(ctx context.Context, p int) ([]*github.Label, int, error) {
		l, resp, e := s.client.Issues.ListLabels(ctx, owner, name, listOpts(p))
		return l, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("github: labels: %w", err)
	}

	if err := section(ctx, s, doc, unavailable, "milestones", func(ctx context.Context, p int) ([]*github.Milestone, int, error) {
		m, resp, e := s.client.Issues.ListMilestones(ctx, owner, name, &github.MilestoneListOptions{State: "all", ListOptions: *listOpts(p)})
		return m, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("github: milestones: %w", err)
	}

	// Issues includes PRs; each Issue carries PullRequestLinks when it is one.
	if err := section(ctx, s, doc, unavailable, "issues", func(ctx context.Context, p int) ([]*github.Issue, int, error) {
		i, resp, e := s.client.Issues.ListByRepo(ctx, owner, name, &github.IssueListByRepoOptions{State: "all", ListOptions: *listOpts(p)})
		return i, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("github: issues: %w", err)
	}

	// number 0 lists every issue/PR conversation comment in the repo.
	if err := section(ctx, s, doc, unavailable, "comments", func(ctx context.Context, p int) ([]*github.IssueComment, int, error) {
		c, resp, e := s.client.Issues.ListComments(ctx, owner, name, 0, &github.IssueListCommentsOptions{ListOptions: *listOpts(p)})
		return c, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("github: comments: %w", err)
	}

	if err := section(ctx, s, doc, unavailable, "pullRequests", func(ctx context.Context, p int) ([]*github.PullRequest, int, error) {
		pr, resp, e := s.client.PullRequests.List(ctx, owner, name, &github.PullRequestListOptions{State: "all", ListOptions: *listOpts(p)})
		return pr, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("github: pull requests: %w", err)
	}

	// number 0 lists every PR review (diff) comment in the repo.
	if err := section(ctx, s, doc, unavailable, "reviewComments", func(ctx context.Context, p int) ([]*github.PullRequestComment, int, error) {
		rc, resp, e := s.client.PullRequests.ListComments(ctx, owner, name, 0, &github.PullRequestListCommentsOptions{ListOptions: *listOpts(p)})
		return rc, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("github: review comments: %w", err)
	}

	if err := section(ctx, s, doc, unavailable, "releases", func(ctx context.Context, p int) ([]*github.RepositoryRelease, int, error) {
		rel, resp, e := s.client.Repositories.ListReleases(ctx, owner, name, listOpts(p))
		return rel, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("github: releases: %w", err)
	}

	if len(unavailable) > 0 {
		doc["unavailable"] = unavailable
	}
	return json.MarshalIndent(doc, "", "  ")
}

// section reads one section's pages into doc under key. A section GitHub answers with 404 or 410 is
// stored empty instead, and its answer recorded in unavailable under the same key.
func section[T any](ctx context.Context, s *Source, doc map[string]any, unavailable map[string]string,
	key string, fetch func(ctx context.Context, page int) ([]T, int, error)) error {
	items, err := collect(ctx, s, fetch)
	if answer, off := turnedOff(err); off {
		doc[key], unavailable[key] = nil, answer
		return nil
	}
	if err != nil {
		return err
	}
	doc[key] = items
	return nil
}

// turnedOff reports whether err is GitHub answering with 404 or 410, and that answer, status and
// message, to record.
func turnedOff(err error) (string, bool) {
	e, ok := errors.AsType[*github.ErrorResponse](err)
	if !ok || e.Response == nil {
		return "", false
	}
	switch code := e.Response.StatusCode; code {
	case http.StatusNotFound, http.StatusGone:
		return fmt.Sprintf("%d %s", code, e.Message), true
	}
	return "", false
}

func listOpts(page int) *github.ListOptions { return &github.ListOptions{Page: page, PerPage: 100} }

func nextPage(resp *github.Response, err error) int {
	if err != nil || resp == nil {
		return 0
	}
	return resp.NextPage
}

// collect paginates a list endpoint, each page through s.call. fetch returns (items, nextPage,
// error) and sends with the context it is given; a nextPage of 0 ends iteration.
func collect[T any](ctx context.Context, s *Source, fetch func(ctx context.Context, page int) ([]T, int, error)) ([]T, error) {
	var all []T
	for page := 1; page != 0; {
		var items []T
		next := 0
		if err := s.call(ctx, func(ctx context.Context) error {
			var err error
			items, next, err = fetch(ctx, page)
			return err
		}); err != nil {
			return nil, err
		}
		all = append(all, items...)
		page = next
	}
	return all, nil
}
