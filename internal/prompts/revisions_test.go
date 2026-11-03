package prompts

import (
	"errors"
	"testing"

	"github.com/AlixWang/agent-relay/internal/store"
)

func TestRecordRevisionsAndBaseDiff(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := RecordTemplateRevisions(st, 100); err != nil {
		t.Fatal(err)
	}
	for _, typ := range Types() {
		rev, err := st.LatestPromptRevision(typ)
		if err != nil || rev == nil {
			t.Fatalf("%s latest: %+v err=%v", typ, rev, err)
		}
		if rev.Revision != TemplateRevision(typ) || rev.BaseRevision != "" {
			t.Fatalf("%s first revision: %+v", typ, rev)
		}
	}
	current := TemplateRevision("muse")
	if err := st.UpsertPromptRevision("next-muse", "muse", current, "old\r\nnew\r\n", 101); err != nil {
		t.Fatal(err)
	}
	d, err := DiffRevisions(st, "muse", "", "next-muse")
	if err != nil {
		t.Fatal(err)
	}
	if d.From != current || d.To != "next-muse" || len(d.Added) == 0 {
		t.Fatalf("base diff: %+v", d)
	}
	if _, err := DiffRevisions(st, "muse", "does-not-exist", "next-muse"); !errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("missing revision error: %v", err)
	}
}
