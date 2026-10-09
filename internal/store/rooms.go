package store

import (
	"database/sql"
	"strings"
)

// Rooms (§6.8): a room is a routing alias with a member list, not a peer. It
// never authenticates, gets no token and no peer row; members receive room
// messages through the ordinary per-identity delivery view (VisibleTo) and the
// console posts into the room as the reserved "operator" identity (§9.5).
//
// One room message is stored once (recipient = the room alias) instead of
// being copied per member: the delivery query decides who may see it, which
// keeps cursors, acks, approvals and the SSE wake path exactly as they are for
// direct messages. Members joining later start from their join sequence, so an
// old room never replays stale instructions at a new member.

// Room is one group chat.
type Room struct {
	ID         string
	Name       string
	Note       string
	CreatedAt  int64
	ArchivedAt int64
}

// RoomMember is one membership row. StartSeq is the message sequence at join
// time: a member sees room messages with seq > StartSeq only.
type RoomMember struct {
	RoomID   string
	PeerID   string
	AddedAt  int64
	StartSeq int64
}

// RoomStats is a console list row: room metadata, its members and the last
// message in the room (one query for the whole page, no per-room N+1).
type RoomStats struct {
	Room
	Members     []string
	Count       int
	LastSeq     int64
	LastSender  string
	LastKind    string
	LastPreview string
	LastAt      int64
}

func (s *sqliteStore) CreateRoom(r *Room) error {
	_, err := s.db.Exec(`INSERT INTO rooms(id,name,note,created_at,archived_at) VALUES(?,?,?,?,?)`,
		r.ID, r.Name, r.Note, r.CreatedAt, r.ArchivedAt)
	return err
}

func (s *sqliteStore) GetRoom(id string) (*Room, error) {
	var r Room
	err := s.db.QueryRow(`SELECT id,COALESCE(name,''),COALESCE(note,''),created_at,COALESCE(archived_at,0)
		FROM rooms WHERE id=?`, id).
		Scan(&r.ID, &r.Name, &r.Note, &r.CreatedAt, &r.ArchivedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ListRoomsWithStats returns newest-activity-first rooms with their member ids
// and last message. Members come from one grouped query, not per room.
func (s *sqliteStore) ListRoomsWithStats(includeArchived bool) ([]*RoomStats, error) {
	q := `SELECT r.id,COALESCE(r.name,''),COALESCE(r.note,''),r.created_at,COALESCE(r.archived_at,0),
		(SELECT COUNT(*) FROM room_members m WHERE m.room_id=r.id),
		COALESCE(lm.seq,0),COALESCE(lm.sender,''),COALESCE(lm.kind,''),
		COALESCE(substr(lm.payload,1,160),''),COALESCE(lm.created_at,0)
		FROM rooms r
		LEFT JOIN messages lm ON lm.seq=(SELECT MAX(seq) FROM messages WHERE recipient=r.id)`
	if !includeArchived {
		q += ` WHERE COALESCE(r.archived_at,0)=0`
	}
	q += ` ORDER BY COALESCE(lm.seq,0) DESC, r.created_at DESC`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RoomStats{}
	index := map[string]*RoomStats{}
	for rows.Next() {
		rs := &RoomStats{}
		if err := rows.Scan(&rs.ID, &rs.Name, &rs.Note, &rs.CreatedAt, &rs.ArchivedAt,
			&rs.Count, &rs.LastSeq, &rs.LastSender, &rs.LastKind, &rs.LastPreview, &rs.LastAt); err != nil {
			return nil, err
		}
		rs.Members = []string{}
		out = append(out, rs)
		index[rs.ID] = rs
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	mrows, err := s.db.Query(`SELECT rm.room_id, rm.peer_id FROM room_members rm
		JOIN rooms r ON r.id=rm.room_id` + roomArchiveFilter(includeArchived) + `
		ORDER BY rm.room_id, rm.added_at`)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var roomID, peerID string
		if err := mrows.Scan(&roomID, &peerID); err != nil {
			return nil, err
		}
		if rs := index[roomID]; rs != nil {
			rs.Members = append(rs.Members, peerID)
		}
	}
	return out, mrows.Err()
}

func roomArchiveFilter(includeArchived bool) string {
	if includeArchived {
		return ""
	}
	return ` WHERE COALESCE(r.archived_at,0)=0`
}

func (s *sqliteStore) UpdateRoom(id, name, note string) error {
	_, err := s.db.Exec(`UPDATE rooms SET name=?, note=? WHERE id=?`, name, note, id)
	return err
}

// SetRoomArchived archives (ts != 0) or restores (ts == 0) a room. Archived
// rooms keep their history but refuse new messages (guard) and stop delivery.
func (s *sqliteStore) SetRoomArchived(id string, ts int64) error {
	_, err := s.db.Exec(`UPDATE rooms SET archived_at=? WHERE id=?`, ts, id)
	return err
}

// AddRoomMember is idempotent: re-adding an existing member keeps the original
// start_seq so a repeat add can never hide history the member already had.
func (s *sqliteStore) AddRoomMember(roomID, peerID string, startSeq, ts int64) error {
	_, err := s.db.Exec(`INSERT INTO room_members(room_id,peer_id,added_at,start_seq) VALUES(?,?,?,?)
		ON CONFLICT(room_id,peer_id) DO NOTHING`, roomID, peerID, ts, startSeq)
	return err
}

func (s *sqliteStore) RemoveRoomMember(roomID, peerID string) error {
	_, err := s.db.Exec(`DELETE FROM room_members WHERE room_id=? AND peer_id=?`, roomID, peerID)
	return err
}

func (s *sqliteStore) ListRoomMembers(roomID string) ([]*RoomMember, error) {
	rows, err := s.db.Query(`SELECT room_id,peer_id,added_at,COALESCE(start_seq,0)
		FROM room_members WHERE room_id=? ORDER BY added_at, peer_id`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RoomMember{}
	for rows.Next() {
		var m RoomMember
		if err := rows.Scan(&m.RoomID, &m.PeerID, &m.AddedAt, &m.StartSeq); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

func (s *sqliteStore) IsRoomMember(roomID, peerID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM room_members WHERE room_id=? AND peer_id=?`,
		roomID, peerID).Scan(&n)
	return n > 0, err
}

// CountRoomMessagesSince counts room messages after the fuse watermark and
// inside the time window — the windowed fuse for long-lived rooms (§6.8).
func (s *sqliteStore) CountRoomMessagesSince(roomID string, sinceSeq, sinceTs int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages
		WHERE recipient=? AND seq>? AND created_at>?`, roomID, sinceSeq, sinceTs).Scan(&n)
	return n, err
}

// AcksForSeqs returns which peers acked each of the given message seqs.
func (s *sqliteStore) AcksForSeqs(seqs []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(seqs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(seqs))
	for _, seq := range seqs {
		args = append(args, seq)
	}
	rows, err := s.db.Query(`SELECT message_seq, peer_id FROM acks WHERE message_seq IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(seqs)), ",")+`) ORDER BY message_seq, acked_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var peer string
		if err := rows.Scan(&seq, &peer); err != nil {
			return nil, err
		}
		out[seq] = append(out[seq], peer)
	}
	return out, rows.Err()
}

// SetSetting stores a console key/value (operator inbox watermark, §9.5).
func (s *sqliteStore) SetSetting(key, value string, ts int64) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, ts)
	return err
}

func (s *sqliteStore) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT COALESCE(value,'') FROM settings WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// InboxMessages pages messages addressed to the operator (replies to the
// console), newest first, with the total count.
func (s *sqliteStore) InboxMessages(limit, offset int) ([]*Message, int, error) {
	limit, offset = clampPage(limit, offset, 20, 200)
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE recipient='operator'`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT seq,id,sender,recipient,kind,in_reply_to,root_id,requires_approval,approval_state,payload,created_at,
		status,op,target,detail,decision,expires_at
		FROM messages WHERE recipient='operator' ORDER BY seq DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

// CountInboxSince counts operator-addressed messages above a read watermark.
func (s *sqliteStore) CountInboxSince(seq int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE recipient='operator' AND seq>?`, seq).Scan(&n)
	return n, err
}
