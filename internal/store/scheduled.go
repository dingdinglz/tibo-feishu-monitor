package store

import (
	"context"
	"encoding/json"
	"errors"

	"tibo-monitor/internal/model"
)

type ScheduledResetFormatter func(model.CodexScheduledReset) (string, error)

// Announcement and execution have separate identities, even with the same API ID.
// Revisions of an announcement retain the first queued snapshot.
func (s *Store) QueueCodexScheduledReset(ctx context.Context, scheduled *model.CodexScheduledReset, format ScheduledResetFormatter) (int, error) {
	if scheduled == nil {
		return 0, nil
	}
	if !scheduled.Valid() {
		return 0, errors.New("invalid scheduled reset")
	}
	if s.beforeDelivery(scheduled.AnnouncedAt) {
		return 0, nil
	}
	raw, err := json.Marshal(scheduled)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO codex_scheduled_resets(id,data) VALUES(?,?)", scheduled.ID, string(raw))
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil || (n == 0 && !s.delivery.Replay) {
		return 0, err
	}
	if n == 0 {
		if err := tx.QueryRowContext(ctx, "SELECT data FROM codex_scheduled_resets WHERE id=?", scheduled.ID).Scan(&raw); err != nil {
			return 0, err
		}
		var saved model.CodexScheduledReset
		if err := json.Unmarshal(raw, &saved); err != nil {
			return 0, err
		}
		scheduled = &saved
		if s.beforeDelivery(scheduled.AnnouncedAt) {
			return 0, nil
		}
	}
	body, err := format(*scheduled)
	if err != nil {
		return 0, err
	}
	result, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO outbox(dedup_key,destination,kind,body,created_at)
SELECT ? || ':' || id,id,'scheduled',?,? FROM destinations WHERE enabled=1`, "scheduled:"+scheduled.ID, body, scheduled.AnnouncedAt.UnixMilli())
	if err != nil {
		return 0, err
	}
	queued, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	added := 0
	if n > 0 || queued > 0 {
		added = 1
	}
	return added, tx.Commit()
}
