package analyze

import "strings"

// Scope narrows analysis to a set of repos (commit-based charts) and/or a
// tracker project (ticket-based charts). The zero value scopes nothing —
// every analysis function's unscoped form delegates to its `…Scoped` form
// with an empty Scope, so org-wide behaviour is unchanged.
type Scope struct {
	Repos          []string // commit repo_name full names ("org/slug")
	TrackerProject string   // ticket project_key ("ENG")
}

// commitFilter returns an "AND <col> IN (?,…)" fragment and args for scoping
// a commit query by repo, or ("", nil) when no repos are set.
func (s Scope) commitFilter(col string) (string, []any) {
	if len(s.Repos) == 0 {
		return "", nil
	}
	ph := make([]string, len(s.Repos))
	args := make([]any, len(s.Repos))
	for i, r := range s.Repos {
		ph[i] = "?"
		args[i] = r
	}
	return " AND " + col + " IN (" + strings.Join(ph, ",") + ")", args
}

// ticketFilter returns an "AND <col> = ?" fragment and args for scoping a
// ticket query by project, or ("", nil) when no project is set.
func (s Scope) ticketFilter(col string) (string, []any) {
	if s.TrackerProject == "" {
		return "", nil
	}
	return " AND " + col + " = ?", []any{s.TrackerProject}
}
