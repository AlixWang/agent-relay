// Package stages is the single ordering source for every work-item stage.
//
// Stage names, ranks and labels live here so the SQL projection order, the
// in-memory attention sort and the console's labels cannot drift apart.
// The console still owns tone/colour because that is presentation, not
// ordering; it never defines a rank of its own.
package stages

import "strings"

// Stage names in attention-first order. The rank of a stage is its index,
// so adding a stage means inserting it at the right place exactly once.
const (
	PersonApproval = "person/approval"
	PersonAnswer   = "person/answer"
	PersonReview   = "person/review"
	PersonBlocked  = "person/blocked"
	PersonFailed   = "person/failed"
	PersonOffline  = "person/offline"
	Working        = "working"
	Idle           = "idle"
	Queued         = "queued"
	Done           = "done"
	Cancelled      = "cancelled"
)

// All is the canonical order used by ListTickets and sortViews.
var All = []string{
	PersonApproval,
	PersonAnswer,
	PersonReview,
	PersonBlocked,
	PersonFailed,
	PersonOffline,
	Working,
	Idle,
	Queued,
	Done,
	Cancelled,
}

// Rank returns the attention-first rank of a stage. Unknown stages sort last
// and never become attention items.
func Rank(stage string) int {
	for i, s := range All {
		if s == stage {
			return i
		}
	}
	return len(All)
}

// Attention reports whether a stage requires a person. It is derived from the
// person/ prefix so a newly added person stage is attention by construction.
func Attention(stage string) bool { return strings.HasPrefix(stage, "person/") }

// Effective applies the non-persisted overlays (presence and an expired
// assignment lease) to a stored stage. It is shared by the store's counts and
// the work service's decoration so both agree on what a person sees.
//
// A lease is a hard delegation boundary: once it expires the executor is no
// longer considered the owner, so an open ticket moves to person/offline even
// when its peer is still sending heartbeats.
func Effective(stored string, assigneeLastSeen, now, onlineSecs int64, leaseExpired bool) (stage string, attention, online bool) {
	if onlineSecs <= 0 {
		onlineSecs = 300
	}
	online = assigneeLastSeen != 0 && now-assigneeLastSeen <= onlineSecs
	stage = stored
	if (stage == Queued || stage == Working) && !online {
		stage = PersonOffline
	}
	if leaseExpired && !Attention(stage) && stage != Done && stage != Cancelled {
		stage = PersonOffline
		online = false
	}
	return stage, Attention(stage), online
}

// OrderCase renders the ORDER BY rank expression for SQL. column is a trusted,
// code-owned identifier (never user input); only the stage literals come from
// this package.
func OrderCase(column string) string {
	var b strings.Builder
	b.WriteString("CASE ")
	b.WriteString(column)
	for i, s := range All {
		b.WriteString(" WHEN '")
		b.WriteString(s)
		b.WriteString("' THEN ")
		b.WriteString(intString(i))
	}
	b.WriteString(" ELSE ")
	b.WriteString(intString(len(All)))
	b.WriteString(" END")
	return b.String()
}

func intString(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
