package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestOpenMigratesPreScopeDatabase reproduces the upgrade path that matters
// most: an existing database whose messages table predates the scope column.
// The index on messages(scope, seq) must not run before the ALTER that adds
// the column, and old rows must be backfilled.
func TestOpenMigratesPreScopeDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE messages (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		id TEXT NOT NULL,
		sender TEXT NOT NULL,
		recipient TEXT NOT NULL,
		kind TEXT DEFAULT 'task',
		in_reply_to TEXT DEFAULT '',
		root_id TEXT DEFAULT '',
		requires_approval INTEGER DEFAULT 0,
		approval_state TEXT DEFAULT 'n/a',
		payload TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		status TEXT DEFAULT '',
		op TEXT DEFAULT '',
		target TEXT DEFAULT '',
		detail TEXT DEFAULT '',
		decision TEXT DEFAULT '',
		expires_at INTEGER DEFAULT 0,
		UNIQUE(sender, id)
	)`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages(id,sender,recipient,kind,root_id,payload,created_at)
		VALUES('old-dm','alice','bob','task','','hello',1),
		      ('old-room','alice','grp_ops','chat','grp_ops','room hello',2)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open old database: %v", err)
	}
	defer st.Close()
	s := st.(*sqliteStore)
	for id, want := range map[string]string{
		"old-dm":   "dm:alice|bob",
		"old-room": "room:grp_ops",
	} {
		var got string
		if err := s.db.QueryRow(`SELECT scope FROM messages WHERE id=?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s scope = %q, want %q", id, got, want)
		}
	}
}
