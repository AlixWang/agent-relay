package prompts

import (
	"errors"
	"fmt"

	"github.com/AlixWang/agent-relay/internal/store"
)

// ErrRevisionQuery marks a malformed/unsatisfiable revision request; callers
// map it to HTTP 400. ErrRevisionNotFound maps to 404.
var (
	ErrRevisionQuery    = errors.New("invalid prompt revision query")
	ErrRevisionNotFound = errors.New("prompt revision not found")
)

// RevisionDiff is one rendered comparison between two immutable snapshots.
type RevisionDiff struct {
	AgentType string   `json:"agent_type"`
	From      string   `json:"from"`
	To        string   `json:"to"`
	Added     []string `json:"added"`
	Removed   []string `json:"removed"`
}

// RecordTemplateRevisions stores one immutable row per supported template.
// Each new row links to the type's previous revision as its base, which is
// what makes /admin/prompts/diff a lookup instead of a guess.
func RecordTemplateRevisions(st store.Store, now int64) error {
	for _, typ := range Types() {
		base := ""
		prev, err := st.LatestPromptRevision(typ)
		if err != nil {
			return fmt.Errorf("latest revision for %s: %w", typ, err)
		}
		if prev != nil {
			base = prev.Revision
		}
		if err := st.UpsertPromptRevision(TemplateRevision(typ), typ, base, TemplateSource(typ), now); err != nil {
			return fmt.Errorf("record revision for %s: %w", typ, err)
		}
	}
	return nil
}

// RevisionCatalog returns the current hash per type and the stored history.
func RevisionCatalog(st store.Store) (map[string]string, []*store.PromptRevision, error) {
	revs, err := st.ListPromptRevisions()
	if err != nil {
		return nil, nil, err
	}
	return TemplateRevisions(), revs, nil
}

// DiffRevisions resolves from/to (to defaults to the current template) and
// renders a line diff. Resolution rules live here, not in the HTTP gateway:
//
//   - to defaults to the current revision of agentType;
//   - from defaults to to's stored base_revision;
//   - if that link is missing (rows created before base_revision existed), the
//     newest other revision of the same type is used;
//   - the two rows must belong to agentType.
func DiffRevisions(st store.Store, agentType, from, to string) (*RevisionDiff, error) {
	if agentType == "" {
		return nil, fmt.Errorf("%w: agent_type is required", ErrRevisionQuery)
	}
	if to == "" {
		to = TemplateRevision(agentType)
	}
	toRow, err := st.GetPromptRevision(to)
	if err != nil {
		return nil, err
	}
	if toRow == nil {
		return nil, fmt.Errorf("%w: %s", ErrRevisionNotFound, to)
	}
	if toRow.AgentType != "" && toRow.AgentType != agentType {
		return nil, fmt.Errorf("%w: revision %s is %s, not %s", ErrRevisionQuery, to, toRow.AgentType, agentType)
	}
	if from == "" {
		from = toRow.BaseRevision
	}
	if from == "" {
		from, err = previousRevision(st, agentType, to)
		if err != nil {
			return nil, err
		}
	}
	if from == "" {
		return nil, fmt.Errorf("%w: no previous revision for %s", ErrRevisionQuery, agentType)
	}
	fromRow, err := st.GetPromptRevision(from)
	if err != nil {
		return nil, err
	}
	if fromRow == nil {
		return nil, fmt.Errorf("%w: %s", ErrRevisionNotFound, from)
	}
	if fromRow.AgentType != "" && fromRow.AgentType != agentType {
		return nil, fmt.Errorf("%w: revision %s is %s, not %s", ErrRevisionQuery, from, fromRow.AgentType, agentType)
	}
	added, removed := LineDiff(fromRow.Content, toRow.Content)
	return &RevisionDiff{AgentType: agentType, From: from, To: to, Added: added, Removed: removed}, nil
}

func previousRevision(st store.Store, agentType, current string) (string, error) {
	revs, err := st.ListPromptRevisions()
	if err != nil {
		return "", err
	}
	for _, rev := range revs {
		if rev.AgentType == agentType && rev.Revision != current {
			return rev.Revision, nil
		}
	}
	return "", nil
}
