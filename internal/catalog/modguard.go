package catalog

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"slices"
)

// The events of the mass-modification guard (specs.md §15.6).
const (
	EventModifyGuard        = "modify-guard"
	EventModifyGuardConfirm = "modify-guard-confirm"
	EventModifyGuardReject  = "modify-guard-reject"
)

// ModifyGuardRead is what the mass-modification guard of a disk reads, from events only, so that no crash can lose a
// hold (specs.md §15.6).
type ModifyGuardRead struct {
	Answer   string        // the kind of the last answer, EventModifyGuardConfirm or EventModifyGuardReject, "" with none
	UpTo     sql.NullInt64 // of that answer
	AnswerID int64         // of that answer's event
	Stands   bool          // a modify-guard event since then
	GuardNS  int64         // the time of that event
	Count    int64         // modified events of the disk within the window, later than the answer, not answered
}

// Answers says whether the last answer covers a copy changed at that change time, and so decides it.
func (g *ModifyGuardRead) Answers(ctimeNS int64) bool {
	return g.UpTo.Valid && ctimeNS <= g.UpTo.Int64
}

// The reads of the mass-modification guard, made at every pass for each disk (specs.md §15.6), as those of the
// mass-deletion guard: never over the whole log (TestGuardReadsUseAnIndex).
const (
	modifyAnswerQuery = `SELECT id, kind, json_extract(data, '$.up_to') FROM events
		WHERE kind IN (?1, ?2) AND json_extract(data, '$.disk') = ?3
		ORDER BY id DESC LIMIT 1`
	modifyGuardSinceQuery = "SELECT ts_ns FROM events WHERE kind = ? AND id > ? AND json_extract(data, '$.disk') = ? ORDER BY id LIMIT 1"
	modifiedCountQuery    = `SELECT count(*) FROM events
		WHERE kind = ?1 AND ts_ns > ?2 AND id > ?3 AND json_extract(data, '$.disk') = ?4 AND json_extract(data, '$.answered') IS NULL`
)

// ReadModifyGuard reads what the mass-modification guard of a disk looks at.
func ReadModifyGuard(ctx context.Context, db *sql.DB, disk string, nowNS, windowNS int64) (*ModifyGuardRead, error) {
	g := &ModifyGuardRead{}
	err := db.QueryRowContext(ctx, modifyAnswerQuery, EventModifyGuardConfirm, EventModifyGuardReject, disk).Scan(&g.AnswerID, &g.Answer, &g.UpTo)
	if err != nil && !isNoRows(err) {
		return nil, fmt.Errorf("guard of %q: %w", disk, err)
	}
	err = db.QueryRowContext(ctx, modifyGuardSinceQuery, EventModifyGuard, g.AnswerID, disk).Scan(&g.GuardNS)
	switch {
	case err == nil:
		g.Stands = true
	case !isNoRows(err):
		return nil, fmt.Errorf("guard of %q: %w", disk, err)
	}
	err = db.QueryRowContext(ctx, modifiedCountQuery, EventModified, nowNS-windowNS, g.AnswerID, disk).Scan(&g.Count)
	if err != nil {
		return nil, fmt.Errorf("guard of %q: %w", disk, err)
	}
	return g, nil
}

// ModifiedPaths gives, in byte order, the paths of the files the mass-modification guard of a disk counts.
func ModifiedPaths(ctx context.Context, db *sql.DB, disk string, nowNS, windowNS, afterID int64) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT json_extract(data, '$.path'), json_extract(data, '$.path_b64') FROM events
		WHERE kind = ?1 AND ts_ns > ?2 AND id > ?3 AND json_extract(data, '$.disk') = ?4 AND json_extract(data, '$.answered') IS NULL`,
		EventModified, nowNS-windowNS, afterID, disk)
	if err != nil {
		return nil, fmt.Errorf("guard of %q: %w", disk, err)
	}
	defer func() { _ = rows.Close() }()
	list := []string{}
	for rows.Next() {
		var text, b64 sql.NullString
		if err := rows.Scan(&text, &b64); err != nil {
			return nil, fmt.Errorf("guard of %q: %w", disk, err)
		}
		if !b64.Valid {
			list = append(list, text.String)
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(b64.String)
		if err != nil {
			return nil, fmt.Errorf("guard of %q: %w", disk, err)
		}
		list = append(list, string(raw))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("guard of %q: %w", disk, err)
	}
	slices.Sort(list)
	return slices.Compact(list), nil
}

// WriteModifyGuardEvent records that the mass-modification guard of a disk tripped.
func WriteModifyGuardEvent(ctx context.Context, db *sql.DB, disk string, nowNS int64) error {
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		return insertEvent(ctx, tx, nowNS, "warning", EventModifyGuard, &eventRow{Disk: disk})
	})
}

// AnswerModifyGuard answers the mass-modification guard of a disk, with a confirm or a reject event whose up_to is the
// time of the modify-guard event that stands. It says nothing was valid when none stands.
func AnswerModifyGuard(ctx context.Context, db *sql.DB, disk, kind string, nowNS int64) (sql.NullInt64, error) {
	var upTo sql.NullInt64
	err := inTransaction(ctx, db, func(tx *sql.Tx) error {
		var last int64
		err := tx.QueryRowContext(ctx, "SELECT id FROM events WHERE kind IN (?, ?) AND json_extract(data, '$.disk') = ? ORDER BY id DESC LIMIT 1",
			EventModifyGuardConfirm, EventModifyGuardReject, disk).Scan(&last)
		if err != nil && !isNoRows(err) {
			return fmt.Errorf("read: %w", err)
		}
		var at int64
		err = tx.QueryRowContext(ctx, modifyGuardSinceQuery, EventModifyGuard, last, disk).Scan(&at)
		switch {
		case isNoRows(err):
			return nil
		case err != nil:
			return fmt.Errorf("read: %w", err)
		}
		upTo = sql.NullInt64{Int64: at, Valid: true}
		return insertEvent(ctx, tx, nowNS, "info", kind, &eventRow{Disk: disk, UpTo: &at})
	})
	if err != nil {
		return upTo, fmt.Errorf("guard of %q: %w", disk, err)
	}
	return upTo, nil
}
