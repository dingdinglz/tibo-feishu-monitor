package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"tibo-monitor/internal/config"
	"tibo-monitor/internal/model"
)

// The API has its own baseline and dedup namespace: switching sources must not
// reuse an X cursor, or hide a confirmed reset just because its post was seen.
func (s *Store) SourceState(ctx context.Context, source string) (ScanState, error) {
	if source != config.SourceCodexResets {
		return s.State(ctx)
	}
	var state ScanState
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='codex_scan'").Scan(&raw)
	if err == sql.ErrNoRows {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	err = json.Unmarshal([]byte(raw), &state)
	return state, err
}

func (s *Store) SetSource(ctx context.Context, source string) error {
	if source == "" {
		source = config.SourceX
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO meta(key,value) VALUES('source',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", source)
	return err
}

type ResetFormatter func(model.CodexReset, bool) (string, error)

func (s *Store) ApplyCodexScan(ctx context.Context, resets []model.CodexReset, state ScanState, now, cutoff time.Time, format ResetFormatter) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	added := 0
	for _, r := range resets {
		if !r.Valid() {
			return 0, errors.New("invalid reset record")
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return 0, err
		}
		result, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO codex_resets(id,data) VALUES(?,?)", r.ID, string(raw))
		if err != nil {
			return 0, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if n == 0 || r.AnnouncedAt.Before(cutoff) || r.AnnouncedAt.Before(state.Baseline) {
			continue
		}
		body, err := format(r, false)
		if err != nil {
			return 0, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO outbox(dedup_key,destination,kind,body,created_at)
SELECT ? || ':' || id,id,'reset',?,? FROM destinations WHERE enabled=1`, "reset:"+r.ID, body, r.AnnouncedAt.UnixMilli())
		if err != nil {
			return 0, err
		}
		added++
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO meta(key,value) VALUES('codex_scan',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(raw)); err != nil {
		return 0, err
	}
	return added, tx.Commit()
}

type WatchFormatter func(model.CodexWatch) (string, error)

// Watches have no upstream ID. The source and observation instant identify an
// event; edits to its probability/text/expiry do not replay the same signal.
// Unlike reset history, a currently active watch is useful even on first run.
func (s *Store) QueueCodexWatch(ctx context.Context, watch *model.CodexWatch, now time.Time, format WatchFormatter) (int, error) {
	if watch == nil {
		return 0, nil
	}
	if !watch.Valid() {
		return 0, errors.New("invalid reset watch")
	}
	if !watch.ExpiresAt.After(now) {
		return 0, nil
	}
	identity, err := json.Marshal(struct {
		Source     model.CodexSource `json:"source"`
		ObservedAt time.Time         `json:"observed_at"`
	}{watch.Source, watch.ObservedAt.UTC()})
	if err != nil {
		return 0, err
	}
	id := fmt.Sprintf("%x", sha256.Sum256(identity))
	raw, err := json.Marshal(watch)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO codex_watches(id,data) VALUES(?,?)", id, string(raw))
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return 0, err
	}
	body, err := format(*watch)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO outbox(dedup_key,destination,kind,body,created_at)
SELECT ? || ':' || id,id,'watch',?,? FROM destinations WHERE enabled=1`, "watch:"+id, body, watch.ObservedAt.UnixMilli()); err != nil {
		return 0, err
	}
	return 1, tx.Commit()
}

func (s *Store) QueueCodexDebug(ctx context.Context, reset model.CodexReset, now time.Time, format ResetFormatter) (int, error) {
	body, err := format(reset, true)
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO outbox(dedup_key,destination,kind,body,created_at)
SELECT ? || ':' || d.id,d.id,'debug',?,? FROM destinations d WHERE enabled=1
AND NOT EXISTS(SELECT 1 FROM outbox o WHERE o.destination=d.id AND o.kind='debug' AND o.status='pending')`,
		fmt.Sprintf("debug-reset:%d:%s", now.UnixNano(), reset.ID), body, now.UnixMilli())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *Store) DailyReportNeeded(ctx context.Context, date string) (bool, error) {
	var needed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM destinations d WHERE d.enabled=1
AND NOT EXISTS(SELECT 1 FROM outbox o WHERE o.dedup_key=? || d.id))`, "daily:"+date+":").Scan(&needed)
	return needed, err
}

// One snapshot per calendar date and destination. Insertion and superseding an
// older undelivered report are atomic; confirmed reset events never expire.
func (s *Store) QueueDailyReport(ctx context.Context, date, body string, now time.Time) (int, error) {
	if _, err := time.Parse("2006-01-02", date); err != nil || body == "" {
		return 0, errors.New("invalid daily report")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	key := "daily:" + date + ":"
	if _, err = tx.ExecContext(ctx, `UPDATE outbox SET status='superseded',last_error='newer_daily_report'
WHERE kind='daily_report' AND status='pending' AND dedup_key<>? || destination
AND destination IN (SELECT id FROM destinations WHERE enabled=1)`, key); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO outbox(dedup_key,destination,kind,body,created_at)
SELECT ? || id,id,'daily_report',?,? FROM destinations WHERE enabled=1`, key, body, now.UnixMilli())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO meta(key,value) VALUES('daily_report_date',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", date); err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}
