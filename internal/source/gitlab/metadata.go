package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"gitdr.io/gitdr/internal/source"
)

// metaSchema versions the metadata document. Audit/reference data, not a faithful
// restorable snapshot (see SPEC §7).
const metaSchema = "gitdr.meta/v1"

// FetchMetadata dumps per-resource project metadata as gitdr.meta/v1 JSON.
func (s *Source) FetchMetadata(ctx context.Context, r source.Repo) ([]byte, error) {
	pid := path.Join(r.Owner, r.Name)
	doc := map[string]any{
		"schema":    metaSchema,
		"host":      r.Host,
		"owner":     r.Owner,
		"name":      r.Name,
		"fetchedAt": time.Now().UTC().Format(time.RFC3339),
	}

	project, _, err := s.client.Projects.GetProject(pid, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("gitlab: get project %s: %w", r.Slug(), err)
	}
	doc["project"] = project

	if doc["labels"], err = collect(func(p int64) ([]*gitlab.Label, int64, error) {
		l, resp, e := s.client.Labels.ListLabels(pid, &gitlab.ListLabelsOptions{ListOptions: listOpts(p)}, gitlab.WithContext(ctx))
		return l, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("gitlab: labels: %w", err)
	}

	if doc["milestones"], err = collect(func(p int64) ([]*gitlab.Milestone, int64, error) {
		m, resp, e := s.client.Milestones.ListMilestones(pid, &gitlab.ListMilestonesOptions{ListOptions: listOpts(p)}, gitlab.WithContext(ctx))
		return m, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("gitlab: milestones: %w", err)
	}

	issues, err := collect(func(p int64) ([]*gitlab.Issue, int64, error) {
		i, resp, e := s.client.Issues.ListProjectIssues(pid, &gitlab.ListProjectIssuesOptions{ListOptions: listOpts(p)}, gitlab.WithContext(ctx))
		return i, nextPage(resp, e), e
	})
	if err != nil {
		return nil, fmt.Errorf("gitlab: issues: %w", err)
	}
	doc["issues"] = issues

	mrs, err := collect(func(p int64) ([]*gitlab.BasicMergeRequest, int64, error) {
		m, resp, e := s.client.MergeRequests.ListProjectMergeRequests(pid, &gitlab.ListProjectMergeRequestsOptions{ListOptions: listOpts(p)}, gitlab.WithContext(ctx))
		return m, nextPage(resp, e), e
	})
	if err != nil {
		return nil, fmt.Errorf("gitlab: merge requests: %w", err)
	}
	doc["mergeRequests"] = mrs

	if doc["releases"], err = collect(func(p int64) ([]*gitlab.Release, int64, error) {
		rel, resp, e := s.client.Releases.ListReleases(pid, &gitlab.ListReleasesOptions{ListOptions: listOpts(p)}, gitlab.WithContext(ctx))
		return rel, nextPage(resp, e), e
	}); err != nil {
		return nil, fmt.Errorf("gitlab: releases: %w", err)
	}

	// Notes (comments) are per issue / per MR, no repo-wide endpoint.
	var notes []*gitlab.Note
	for _, iss := range issues {
		ns, e := collect(func(p int64) ([]*gitlab.Note, int64, error) {
			n, resp, e := s.client.Notes.ListIssueNotes(pid, iss.IID, &gitlab.ListIssueNotesOptions{ListOptions: listOpts(p)}, gitlab.WithContext(ctx))
			return n, nextPage(resp, e), e
		})
		if e != nil {
			return nil, fmt.Errorf("gitlab: issue %d notes: %w", iss.IID, e)
		}
		notes = append(notes, ns...)
	}
	for _, mr := range mrs {
		ns, e := collect(func(p int64) ([]*gitlab.Note, int64, error) {
			n, resp, e := s.client.Notes.ListMergeRequestNotes(pid, mr.IID, &gitlab.ListMergeRequestNotesOptions{ListOptions: listOpts(p)}, gitlab.WithContext(ctx))
			return n, nextPage(resp, e), e
		})
		if e != nil {
			return nil, fmt.Errorf("gitlab: mr %d notes: %w", mr.IID, e)
		}
		notes = append(notes, ns...)
	}
	doc["notes"] = notes

	return json.MarshalIndent(doc, "", "  ")
}

func listOpts(page int64) gitlab.ListOptions { return gitlab.ListOptions{Page: page, PerPage: 100} }

func nextPage(resp *gitlab.Response, err error) int64 {
	if err != nil || resp == nil {
		return 0
	}
	return resp.NextPage
}

// collect paginates a list endpoint. fetch returns (items, nextPage, error); a
// nextPage of 0 ends iteration.
func collect[T any](fetch func(page int64) ([]T, int64, error)) ([]T, error) {
	var all []T
	for page := int64(1); page != 0; {
		items, next, err := fetch(page)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		page = next
	}
	return all, nil
}
