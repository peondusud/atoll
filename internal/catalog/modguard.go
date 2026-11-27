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

// WindowDay is the window of the mass-modification guard whose count tripped a hold, in the data of its modify-guard
// event and of the answer to it, when it is the day's, modifies.day_guard (specs.md §9, §15.6). A hold its own window
// tripped, modifies.mass_guard.window, says none.
const WindowDay = "day"

// ModifyGuardRead is what the mass-modification guard of a disk reads, from events only, so that no crash can lose a
// hold, and no restart a count (specs.md §15.6).
type ModifyGuardRead struct {
	Answer   string        // the kind of the last answer, EventModifyGuardConfirm or EventModifyGuardReject, "" with none
	UpTo     sql.NullInt64 // of that answer
	AnswerID int64         // of that answer's event
	// Lapsed says that the answer was given more than the guard's window ago: it decides nothing any more.
	Lapsed  bool
	Stands  bool  // a modify-guard event since then
	GuardNS int64 // the time of that event
	// Window is the window whose count tripped the hold that stands: WindowDay for the day's, "" for the guard's own.
	Window string
	Count  int64 // modified events of the disk within the window, later than the answer, not answered
	// DayAnswerID is the event of the last answer to a hold the day tripped, 0 with none: the day counts after it, so
	// that an answer to a hold the window tripped, which spoke of the window's files, leaves the day's count as it was.
	DayAnswerID int64
	DayCount    int64 // modified events of the disk within the day, later than DayAnswerID, not answered
	// RejectUpTo is the greatest up_to of the rejects of the guard, whatever answer came after them, invalid with none.
	RejectUpTo sql.NullInt64
}

// Answers says whether the last answer covers a copy changed at that change time, and so decides it: one given
// within the guard's window, as an answer speaks of what the guard held when it was given, and stands for no change
// found long after (specs.md §15.6).
func (g *ModifyGuardRead) Answers(ctimeNS int64) bool {
	return g.UpTo.Valid && !g.Lapsed && ctimeNS <= g.UpTo.Int64
}

// Rejected says whether a reject covers a copy changed at that change time: one made at or before the greatest up_to of
// the rejects, which each later reject holds but for a clock set back. A reject never lapses, whatever answer came
// after it, however long ago it was given: what it covered is a conflict at every pass, which overwrites nothing, until
// atoll conflicts resolve settles it (specs.md §15.6; conformance/models/README.md, flaw 27, the maintainer's remedy
// reject-kept, 2026-11-25).
func (g *ModifyGuardRead) Rejected(ctimeNS int64) bool {
	return g.RejectUpTo.Valid && ctimeNS <= g.RejectUpTo.Int64
}

// The reads of the mass-modification guard, made at every pass for each disk (specs.md §15.6), as those of the
// mass-deletion guard: never over the whole log (TestGuardReadsUseAnIndex).
const (
	modifyAnswerQuery = `SELECT id, kind, json_extract(data, '$.up_to'), ts_ns FROM events
		WHERE kind IN (?1, ?2) AND json_extract(data, '$.disk') = ?3
		ORDER BY id DESC LIMIT 1`
	modifyDayAnswerQuery = `SELECT id FROM events
		WHERE kind IN (?1, ?2) AND json_extract(data, '$.disk') = ?3 AND json_extract(data, '$.window') = ?4
		ORDER BY id DESC LIMIT 1`
	modifyGuardSinceQuery = `SELECT ts_ns, COALESCE(json_extract(data, '$.window'), '') FROM events
		WHERE kind = ? AND id > ? AND json_extract(data, '$.disk') = ? ORDER BY id LIMIT 1`
	modifiedCountQuery = `SELECT count(*) FROM events
		WHERE kind = ?1 AND ts_ns > ?2 AND id > ?3 AND json_extract(data, '$.disk') = ?4 AND json_extract(data, '$.answered') IS NULL`
	modifyRejectQuery = `SELECT max(json_extract(data, '$.up_to')) FROM events
		WHERE kind = ?1 AND json_extract(data, '$.disk') = ?2`
)

// ReadModifyGuard reads what the mass-modification guard of a disk looks at: over its window, windowNS, and over its
// day, dayNS (specs.md §15.6).
func ReadModifyGuard(ctx context.Context, db *sql.DB, disk string, nowNS, windowNS, dayNS int64) (*ModifyGuardRead, error) {
	g := &ModifyGuardRead{}
	var givenNS int64
	err := db.QueryRowContext(ctx, modifyAnswerQuery, EventModifyGuardConfirm, EventModifyGuardReject, disk).Scan(&g.AnswerID, &g.Answer, &g.UpTo, &givenNS)
	switch {
	case err == nil:
		g.Lapsed = nowNS-givenNS > windowNS
	case !isNoRows(err):
		return nil, fmt.Errorf("guard of %q: %w", disk, err)
	}
	err = db.QueryRowContext(ctx, modifyGuardSinceQuery, EventModifyGuard, g.AnswerID, disk).Scan(&g.GuardNS, &g.Window)
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
	err = db.QueryRowContext(ctx, modifyDayAnswerQuery, EventModifyGuardConfirm, EventModifyGuardReject, disk, WindowDay).Scan(&g.DayAnswerID)
	if err != nil && !isNoRows(err) {
		return nil, fmt.Errorf("guard of %q: %w", disk, err)
	}
	err = db.QueryRowContext(ctx, modifiedCountQuery, EventModified, nowNS-dayNS, g.DayAnswerID, disk).Scan(&g.DayCount)
	if err != nil {
		return nil, fmt.Errorf("guard of %q: %w", disk, err)
	}
	if err := db.QueryRowContext(ctx, modifyRejectQuery, EventModifyGuardReject, disk).Scan(&g.RejectUpTo); err != nil {
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

// WriteModifyGuardEvent records that the mass-modification guard of a disk tripped, on the count of a window: WindowDay,
// or "" for the guard's own.
func WriteModifyGuardEvent(ctx context.Context, db *sql.DB, disk, window string, nowNS int64) error {
	return inTransaction(ctx, db, func(tx *sql.Tx) error {
		return insertEvent(ctx, tx, nowNS, "warning", EventModifyGuard, &eventRow{Disk: disk, Window: window})
	})
}

// AnswerModifyGuard answers the mass-modification guard of a disk, with a confirm or a reject event whose up_to is the
// time of the modify-guard event that stands, and whose window is that event's. It says nothing was valid when none
// stands.
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
		var window string
		err = tx.QueryRowContext(ctx, modifyGuardSinceQuery, EventModifyGuard, last, disk).Scan(&at, &window)
		switch {
		case isNoRows(err):
			return nil
		case err != nil:
			return fmt.Errorf("read: %w", err)
		}
		upTo = sql.NullInt64{Int64: at, Valid: true}
		return insertEvent(ctx, tx, nowNS, "info", kind, &eventRow{Disk: disk, UpTo: &at, Window: window})
	})
	if err != nil {
		return upTo, fmt.Errorf("guard of %q: %w", disk, err)
	}
	return upTo, nil
}
