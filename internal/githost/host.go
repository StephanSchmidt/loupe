// Package githost defines the provider-neutral interface Loupe uses to talk
// to git-hosting services (Bitbucket Cloud, GitLab, GitHub, …). Concrete
// implementations live under subpackages — `bitbucket` is the only one in
// v0. Domain consumers (ingest, status, render) depend only on this package.
package githost

import (
	"context"
	"iter"
	"time"
)

// GitHost is the provider abstraction. Constructors return this interface
// so callers can't accidentally bind to a concrete client type.
type GitHost interface {
	// Name returns the provider identifier persisted on each row, e.g.
	// "bitbucket-cloud". Stable across versions — schemas key on it.
	Name() string

	// ListWorkspaces returns every workspace the credential can see.
	// Small lists in practice; a slice is fine.
	ListWorkspaces(ctx context.Context) ([]Workspace, error)

	// ListRepos returns every repo under a workspace.
	ListRepos(ctx context.Context, workspaceSlug string) ([]Repo, error)

	// ListCommits streams every commit in repo with committed_at > since.
	// If since.IsZero() the full history is streamed. The iterator yields
	// (Commit, nil) for each row; if it yields (Commit{}, err) the caller
	// must stop iterating.
	ListCommits(ctx context.Context, repo RepoRef, since time.Time) iter.Seq2[Commit, error]

	// ListPullRequests streams every PR in repo updated after since.
	ListPullRequests(ctx context.Context, repo RepoRef, since time.Time) iter.Seq2[PullRequest, error]

	// ListPRCommits returns the pre-squash commits associated with a PR.
	// Used to recover Co-Authored-By trailers that get dropped by squash
	// merges on the merge commit.
	ListPRCommits(ctx context.Context, repo RepoRef, prID string) ([]Commit, error)
}

// Workspace is a top-level grouping (Bitbucket workspace, GitLab group,
// GitHub org).
type Workspace struct {
	Slug string
	Name string
}

// RepoRef is the minimum address needed to fetch resources under a repo.
type RepoRef struct {
	Workspace string
	Slug      string
}

// FullName returns the canonical "workspace/slug" form used as a primary
// key in the repos table.
func (r RepoRef) FullName() string {
	return r.Workspace + "/" + r.Slug
}

// Repo is the discovery-time record stored in the repos table.
type Repo struct {
	RepoRef
	Name string
}

// Commit is the neutral commit record. Message includes subject + body
// joined with a blank line — the same shape git's `%s\n\n%b` produces — so
// the trailer regex in internal/analyze/signals.go works unchanged.
type Commit struct {
	SHA         string
	AuthorEmail string
	AuthorName  string
	CommittedAt time.Time
	Message     string
	ParentCount int
}

// PullRequest is the neutral PR record.
//
// AuthorLogin is the host-side handle (GitHub login, Bitbucket nickname);
// it carries the `[bot]` suffix for GitHub App identities and is the only
// reliable bot signal we have on PR rows, since AuthorEmail is often blank
// or, on GitHub, populated with the login as a fallback.
//
// AuthorIsBot is the host's authoritative bot flag — GitHub's REST API
// labels App-bot users with `type: "Bot"`. Bitbucket Cloud has no
// equivalent and always leaves this false.
type PullRequest struct {
	ID                string
	Title             string
	State             string // "OPEN" | "MERGED" | "DECLINED" | "SUPERSEDED"
	AuthorEmail       string
	AuthorLogin       string
	AuthorIsBot       bool
	SourceBranch      string
	DestinationBranch string
	CreatedAt         time.Time
	MergedAt          *time.Time
	ClosedAt          *time.Time
	MergeCommitSHA    string
	Labels            []string
}
