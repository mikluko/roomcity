package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// fileMeta is what the events table keeps of a File besides its name.
type fileMeta struct {
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
	Event     string `json:"event"`
}

// fileFrom is the File an events row of kind eventFile holds.
func fileFrom(seq uint64, at int64, name string, message []byte) File {
	f := File{Seq: seq, Time: time.Unix(0, at), Name: name}
	var meta fileMeta
	if json.Unmarshal(message, &meta) == nil {
		f.MediaType, f.Size, f.Event = meta.MediaType, meta.Size, meta.Event
	}
	return f
}

// RecordFile records, in the session's stream, a file it sent to its room, and
// returns the Seq of the record. Seq, Time and the bytes are not the caller's:
// the record is what the file was and the event that carries it.
func (r *Registry) RecordFile(ctx context.Context, name string, f File) (uint64, error) {
	var seq uint64
	err := r.tx(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRow(`SELECT 1 FROM sessions WHERE name = ?`, name).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var err error
		seq, err = r.appendEvent(tx, name, f)
		return err
	})
	if err == nil {
		r.changed()
	}
	return seq, err
}

// KnownToken reports whether token is the token of some session that has not
// ended. It is for a check that has to come before a call's body is read, when
// the session it names is not yet known; the call's own check, Authorize,
// still decides.
func (r *Registry) KnownToken(token string) bool {
	if token == "" {
		return false
	}
	var one int
	err := r.db.QueryRow(`SELECT 1 FROM sessions WHERE token_hash = ? AND state <> ?`, hashToken(token), Ended).Scan(&one)
	return err == nil
}
