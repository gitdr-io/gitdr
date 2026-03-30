package github

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/go-github/v88/github"

	"gitdr.io/gitdr/internal/source"
)

// metaSchema versions the metadata document. It is audit/reference data, not a
// faithful, restorable snapshot (see SPEC §7), the API cannot recreate original
// numbers, authors, or timestamps.
const metaSchema = "gitdr.meta/v1"

// FetchMetadata dumps per-resource metadata as gitdr.meta/v1 JSON using App-compatible
// per-resource REST endpoints (never the Migrations API).
func (s *Source) FetchMetadata(ctx context.Context, r source.Repo) ([]byte, error) {
	owner, name := r.Owner, r.Name
	doc := map[string]any{
		"schema":    metaSchema,
		"host":      r.Host,
		"owner":     owner,
		"name":      name,
		"fetchedAt": time.Now().UTC().Format(time.RFC3339),
	}

	repo, _, err := s.client.Repositories.Get(ctx, owner, name)
	if err != nil {
		return nil, fmt.Errorf("github: get repo %s: %w", r.Slug(), err)
	}
	doc["repo"] = repo

	if doc["labels"], err = collect(func(p int) ([]*github.Label, int, error) {
		l, resp, e := s.client.Issues.ListLabels(ctx, owner, name, listOpts(p))
		return l, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("github: labels: %w", err)
	}

	if doc["milestones"], err = collect(func(p int) ([]*github.Milestone, int, error) {
		m, resp, e := s.client.Issues.ListMilestones(ctx, owner, name, &github.MilestoneListOptions{State: "all", ListOptions: *listOpts(p)})
		return m, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("github: milestones: %w", err)
	}

	// Issues includes PRs; each Issue carries PullRequestLinks when it is one.
	if doc["issues"], err = collect(func(p int) ([]*github.Issue, int, error) {
		i, resp, e := s.client.Issues.ListByRepo(ctx, owner, name, &github.IssueListByRepoOptions{State: "all", ListOptions: *listOpts(p)})
		return i, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("github: issues: %w", err)
	}

	// number 0 lists every issue/PR conversation comment in the repo.
	if doc["comments"], err = collect(func(p int) ([]*github.IssueComment, int, error) {
		c, resp, e := s.client.Issues.ListComments(ctx, owner, name, 0, &github.IssueListCommentsOptions{ListOptions: *listOpts(p)})
		return c, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("github: comments: %w", err)
	}

	if doc["pullRequests"], err = collect(func(p int) ([]*github.PullRequest, int, error) {
		pr, resp, e := s.client.PullRequests.List(ctx, owner, name, &github.PullRequestListOptions{State: "all", ListOptions: *listOpts(p)})
		return pr, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("github: pull requests: %w", err)
	}

	// number 0 lists every PR review (diff) comment in the repo.
	if doc["reviewComments"], err = collect(func(p int) ([]*github.PullRequestComment, int, error) {
		rc, resp, e := s.client.PullRequests.ListComments(ctx, owner, name, 0, &github.PullRequestListCommentsOptions{ListOptions: *listOpts(p)})
		return rc, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("github: review comments: %w", err)
	}

	if doc["releases"], err = collect(func(p int) ([]*github.RepositoryRelease, int, error) {
		rel, resp, e := s.client.Repositories.ListReleases(ctx, owner, name, listOpts(p))
		return rel, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("github: releases: %w", err)
	}

	return json.MarshalIndent(doc, "", "  ")
}

func listOpts(page int) *github.ListOptions { return &github.ListOptions{Page: page, PerPage: 100} }

func nextPage(resp *github.Response, err error) int {
	if err != nil || resp == nil {
		return 0
	}
	return resp.NextPage
}

// collect paginates a list endpoint. fetch returns (items, nextPage, error); a
// nextPage of 0 ends iteration.
func collect[T any](fetch func(page int) ([]T, int, error)) ([]T, error) {
	var all []T
	for page := 1; page != 0; {
		items, next, err := fetch(page)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		page = next
	}
	return all, nil
}
