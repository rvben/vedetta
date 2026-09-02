package storage

import (
	"database/sql"
	"log/slog"
	"runtime"
	"strings"
	"time"
)

// eventDeleteBatchSize bounds how many events one retention transaction removes.
// SQLite allows a single writer, so a transaction covering every expired event
// holds the write lock for the whole sweep and blocks the recorder behind it.
// Each batch commits on its own, which releases the lock between batches and
// keeps the work already done when a later batch fails. The size also stays well
// under SQLite's bind-parameter limit, because a batch binds one parameter per
// event id.
const eventDeleteBatchSize = 500

// DeleteEventsOlderThan removes every event recorded before cutoff and
// reconciles the activities those events belonged to. It works in bounded
// batches rather than one transaction.
func (d *DB) DeleteEventsOlderThan(cutoff time.Time) error {
	deleted, err := d.deleteEventsOlderThan(cutoff, eventDeleteBatchSize)
	if deleted > 0 {
		slog.Debug("deleted expired events", "count", deleted, "cutoff", utc(cutoff))
	}
	return err
}

// deleteEventsOlderThan deletes in batches until no event before cutoff is left
// and reports how many rows were removed. The count covers the batches that
// committed, so it stays accurate when a later batch fails.
func (d *DB) deleteEventsOlderThan(cutoff time.Time, batchSize int) (int64, error) {
	if batchSize <= 0 {
		batchSize = eventDeleteBatchSize
	}
	var total int64
	for {
		deleted, err := d.deleteEventBatch(cutoff, batchSize)
		total += deleted
		if err != nil {
			return total, err
		}
		if deleted < int64(batchSize) {
			return total, nil
		}
		// Committing the batch releases the write lock. Yielding here gives a
		// writer waiting on it a turn before the next batch takes it again.
		runtime.Gosched()
	}
}

// deleteEventBatch removes up to batchSize expired events in one transaction and
// returns how many rows it deleted. A failure rolls the batch back, leaving the
// events it covers for the next run.
func (d *DB) deleteEventBatch(cutoff time.Time, batchSize int) (int64, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	ids, err := expiredEventIDs(tx, cutoff, batchSize)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	// Deleting an event cascades to its activity_events link, so the activities
	// that need recomputing are read while the links still exist.
	activityIDs, err := activityIDsForEvents(tx, ids)
	if err != nil {
		return 0, err
	}

	res, err := tx.Exec("DELETE FROM events WHERE id IN ("+idPlaceholders(len(ids))+")", idArgs(ids)...)
	if err != nil {
		return 0, err
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	for _, activityID := range activityIDs {
		if _, err := reconcileActivityTx(tx, activityID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

// expiredEventIDs returns the oldest event ids before cutoff, at most limit of
// them. Oldest first means repeated calls walk forward through the expired
// range instead of revisiting the same rows.
func expiredEventIDs(tx *sql.Tx, cutoff time.Time, limit int) ([]string, error) {
	rows, err := tx.Query(`
		SELECT id FROM events
		WHERE timestamp < ?
		ORDER BY timestamp
		LIMIT ?`, utc(cutoff), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// activityIDsForEvents returns the distinct activities the given events belong
// to.
func activityIDsForEvents(tx *sql.Tx, eventIDs []string) ([]string, error) {
	rows, err := tx.Query(`
		SELECT DISTINCT activity_id
		FROM activity_events
		WHERE event_id IN (`+idPlaceholders(len(eventIDs))+`)`, idArgs(eventIDs)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var activityIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		activityIDs = append(activityIDs, id)
	}
	return activityIDs, rows.Err()
}

// idPlaceholders builds the "?, ?, ?" list for an IN clause over n ids.
func idPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// idArgs converts ids into query arguments.
func idArgs(ids []string) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}
