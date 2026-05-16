// Package tracker defines the provider-neutral interface Loupe uses to talk
// to issue trackers (Jira Cloud, Linear, GitHub Issues, …). Concrete
// implementations live under subpackages — `jira` is the only one in v0.
package tracker

import (
	"context"
	"iter"
	"time"
)

// Tracker is the provider abstraction.
type Tracker interface {
	// Name returns the provider identifier persisted on each row, e.g.
	// "jira-cloud".
	Name() string

	// ListProjects returns every project the credential can see.
	ListProjects(ctx context.Context) ([]Project, error)

	// ListIssues streams every issue in a project updated after since.
	ListIssues(ctx context.Context, projectKey string, since time.Time) iter.Seq2[Issue, error]
}

// Project is the discovery-time record stored in the tracker_projects table.
type Project struct {
	Key  string // e.g. "ENG"
	Name string
}

// Issue is the neutral ticket record.
type Issue struct {
	ID            string // internal identifier (Jira: numeric)
	Key           string // human-readable, e.g. "ENG-123"
	ProjectKey    string
	Title         string
	Type          string
	Status        string
	AssigneeEmail string
	CreatedAt     time.Time
	ResolvedAt    *time.Time
	ClosedAt      *time.Time
	Estimate      float64
	// Transitions are status change events on the issue, oldest-first.
	// Populated by trackers that expose a changelog (Jira); left empty for
	// providers that don't (GitHub Issues in v0.3).
	Transitions []Transition
}

// Transition is a single status change on an issue.
type Transition struct {
	At         time.Time
	FromStatus string
	ToStatus   string
}
