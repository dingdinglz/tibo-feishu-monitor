package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"tibo-monitor/internal/config"
	"tibo-monitor/internal/model"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type ScanState struct {
	Initialized        bool      `json:"initialized"`
	Baseline           time.Time `json:"baseline"`
	LastComplete       time.Time `json:"last_complete"`
	LastRequestSuccess time.Time `json:"last_request_success"`
	Cursor             string    `json:"cursor"`
	ScanSince          time.Time `json:"scan_since"`
	ScanStarted        time.Time `json:"scan_started"`
}

type Job struct {
	ID          int64
	Destination string
	DedupKey    string
	Text        string
	Kind        string
	Attempts    int
}

type Status struct {
	Scan         ScanState           `json:"scan"`
	Posts        int                 `json:"posts"`
	Pending      int                 `json:"pending"`
	Sent         int                 `json:"sent"`
	Destinations []DestinationStatus `json:"destinations"`
}

type DestinationStatus struct {
	ID       string `json:"id"`
	Enabled  bool   `json:"enabled"`
	Blocked  bool   `json:"blocked"`
	Pending  int    `json:"pending"`
	LastSent int64  `json:"last_sent_unix_ms"`
}

const schema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS destinations (
 id TEXT PRIMARY KEY, enabled INTEGER NOT NULL, blocked INTEGER NOT NULL DEFAULT 0,
 next_allowed INTEGER NOT NULL DEFAULT 0, last_sent INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS posts (
 id TEXT PRIMARY KEY, created_at INTEGER NOT NULL, discovered_at INTEGER NOT NULL, data TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS posts_created ON posts(created_at, length(id), id);
CREATE TABLE IF NOT EXISTS reset_assessments (key TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS contexts (id TEXT PRIMARY KEY, data TEXT NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS outbox (
 id INTEGER PRIMARY KEY AUTOINCREMENT, dedup_key TEXT NOT NULL UNIQUE,
 destination TEXT NOT NULL REFERENCES destinations(id), post_id TEXT REFERENCES posts(id),
 part INTEGER NOT NULL DEFAULT 0, kind TEXT NOT NULL, body TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL,
 sent_at INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS outbox_pending ON outbox(destination, status, created_at, part);
CREATE TABLE IF NOT EXISTS faults (
 key TEXT PRIMARY KEY, failures INTEGER NOT NULL, alerted INTEGER NOT NULL DEFAULT 0,
 last_alert INTEGER NOT NULL DEFAULT 0
);
`

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON", schema} {
		if _, err = db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &Store{db: db}, nil
}

// OpenReadOnly is used by status and never creates tables or changes permissions.
func OpenReadOnly(path string) (*Store, error) {
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Config contains credentials; only stable IDs and enabled flags are persisted here.
// Restart explicitly retries blocked targets after the operator has corrected configuration.
func (s *Store) SyncDestinations(ctx context.Context, target string, ds []config.Destination) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='target'").Scan(&existing)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil && existing != target {
		return errors.New("database belongs to a different target; use a separate database")
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO meta(key,value) VALUES('target',?)", target); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE destinations SET enabled=0"); err != nil {
		return err
	}
	for _, d := range ds {
		_, err = tx.ExecContext(ctx, `INSERT INTO destinations(id,enabled) VALUES(?,?)
ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled, blocked=0`, d.ID, d.IsEnabled())
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) State(ctx context.Context) (ScanState, error) {
	var state ScanState
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='scan'").Scan(&raw)
	if err == sql.ErrNoRows {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	err = json.Unmarshal([]byte(raw), &state)
	return state, err
}

type Formatter func(model.Post) ([]string, error)

// ApplyScan atomically persists discoveries, per-destination parts and scan progress.
// Existing posts NEVER create tasks for a newly registered destination.
func (s *Store) ApplyScan(ctx context.Context, result model.FetchResult, state ScanState, now, cutoff time.Time, format Formatter) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT id FROM destinations WHERE enabled=1")
	if err != nil {
		return 0, err
	}
	var destinations []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		destinations = append(destinations, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	added := 0
	for _, p := range result.Posts {
		raw, err := json.Marshal(p)
		if err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO posts(id,created_at,discovered_at,data) VALUES(?,?,?,?)", p.ID, p.CreatedAt.UnixMilli(), now.UnixMilli(), string(raw))
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if n == 0 || p.CreatedAt.Before(cutoff) || p.CreatedAt.Before(state.Baseline) {
			continue
		}
		parts, err := format(p)
		if err != nil {
			return 0, err
		}
		if len(parts) == 0 {
			return 0, errors.New("formatter returned no parts")
		}
		for _, dest := range destinations {
			for part, text := range parts {
				key := fmt.Sprintf("post:%s:%s:%d", p.ID, dest, part)
				_, err = tx.ExecContext(ctx, `INSERT INTO outbox(dedup_key,destination,post_id,part,kind,body,created_at) VALUES(?,?,?,?, 'post',?,?)`, key, dest, p.ID, part, text, p.CreatedAt.UnixMilli())
				if err != nil {
					return 0, err
				}
			}
		}
		added++
	}
	for _, c := range result.Contexts {
		if !c.Available || c.ID == "" {
			continue
		}
		// Only one short context excerpt is ever displayed. Bound the cache's
		// stdin/storage footprint independently of X long-form post sizes.
		if runes := []rune(c.Text); len(runes) > 400 {
			c.Text = string(runes[:400]) + "…"
		}
		raw, err := json.Marshal(c)
		if err != nil {
			return 0, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO contexts(id,data,updated_at) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data,updated_at=excluded.updated_at`, c.ID, string(raw), now.UnixMilli())
		if err != nil {
			return 0, err
		}
	}
	// The cache is bounded; post/outbox dedup tombstones are intentionally retained.
	if _, err = tx.ExecContext(ctx, "DELETE FROM contexts WHERE id NOT IN (SELECT id FROM contexts ORDER BY updated_at DESC LIMIT 500)"); err != nil {
		return 0, err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO meta(key,value) VALUES('scan',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(raw)); err != nil {
		return 0, err
	}
	return added, tx.Commit()
}

// QueueDebug creates a separate test batch without inserting/modifying posts or
// normal delivery dedup keys. At most one pending debug batch per destination
// prevents a failing webhook from accumulating identical tests every poll.
func (s *Store) QueueDebug(ctx context.Context, p model.Post, now time.Time, format Formatter) (int, error) {
	parts, err := format(p)
	if err != nil {
		return 0, err
	}
	if len(parts) == 0 {
		return 0, errors.New("formatter returned no parts")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT d.id FROM destinations d WHERE d.enabled=1
AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.destination=d.id AND o.kind='debug' AND o.status='pending')`)
	if err != nil {
		return 0, err
	}
	var destinations []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		destinations = append(destinations, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, dest := range destinations {
		inserted := false
		for part, body := range parts {
			key := fmt.Sprintf("debug:%d:%s:%s:%d", now.UnixNano(), p.ID, dest, part)
			res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO outbox(dedup_key,destination,part,kind,body,created_at) VALUES(?,?,?,'debug',?,?)`, key, dest, part, body, now.UnixMilli())
			if err != nil {
				return 0, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return 0, err
			}
			inserted = inserted || n > 0
		}
		if inserted {
			queued++
		}
	}
	return queued, tx.Commit()
}

// UpgradePendingPosts collapses legacy text fragments into one pending card per
// post/destination. Successful deliveries stay untouched, and the retained task
// keeps its retry timing/attempts. Invoke before starting delivery workers.
func (s *Store) UpgradePendingPosts(ctx context.Context, format Formatter) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT o.id,o.post_id,o.destination,o.body,p.data
FROM outbox o JOIN posts p ON p.id=o.post_id WHERE o.kind='post' AND o.status='pending'
ORDER BY o.post_id,o.destination,o.part,o.id`)
	if err != nil {
		return 0, err
	}
	type group struct {
		id                             int64
		postID, destination, body, raw string
		count                          int
	}
	var groups []group
	for rows.Next() {
		var g group
		if err := rows.Scan(&g.id, &g.postID, &g.destination, &g.body, &g.raw); err != nil {
			rows.Close()
			return 0, err
		}
		if n := len(groups); n > 0 && groups[n-1].postID == g.postID && groups[n-1].destination == g.destination {
			groups[n-1].count++
		} else {
			g.count = 1
			groups = append(groups, g)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	updated := 0
	for _, g := range groups {
		var p model.Post
		if err := json.Unmarshal([]byte(g.raw), &p); err != nil || p.ID != g.postID {
			return 0, errors.New("invalid stored post during card upgrade")
		}
		bodies, err := format(p)
		if err != nil {
			return 0, err
		}
		if len(bodies) != 1 {
			return 0, errors.New("card formatter must return exactly one message")
		}
		if g.count == 1 && g.body == bodies[0] {
			continue
		}
		if _, err = tx.ExecContext(ctx, "UPDATE outbox SET body=? WHERE id=?", bodies[0], g.id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE outbox SET status='superseded',last_error='consolidated_into_card'
WHERE kind='post' AND status='pending' AND post_id=? AND destination=? AND id<>?`, g.postID, g.destination, g.id); err != nil {
			return 0, err
		}
		updated++
	}
	return updated, tx.Commit()
}

func (s *Store) Contexts(ctx context.Context) (map[string]model.Context, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,data FROM contexts ORDER BY updated_at DESC LIMIT 500")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]model.Context{}
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var c model.Context
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// Claim leaves the task pending until acknowledgement. A crash may resend an
// uncertain attempt, but cannot silently lose it. A process lock ensures one owner.
func (s *Store) Claim(ctx context.Context, dest string, now time.Time, interval time.Duration) (*Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var enabled, blocked bool
	var allowed int64
	err = tx.QueryRowContext(ctx, "SELECT enabled,blocked,next_allowed FROM destinations WHERE id=?", dest).Scan(&enabled, &blocked, &allowed)
	if err != nil {
		return nil, err
	}
	if !enabled || blocked || allowed > now.UnixMilli() {
		return nil, nil
	}
	var j Job
	var next int64
	err = tx.QueryRowContext(ctx, `SELECT id,destination,dedup_key,body,kind,attempts,next_attempt FROM outbox WHERE destination=? AND status='pending'
ORDER BY CASE kind WHEN 'alert' THEN 0 WHEN 'debug' THEN 2 ELSE 1 END, created_at, length(post_id), post_id, part, id LIMIT 1`, dest).Scan(&j.ID, &j.Destination, &j.DedupKey, &j.Text, &j.Kind, &j.Attempts, &next)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if next > now.UnixMilli() {
		return nil, nil
	}
	j.Attempts++
	if _, err = tx.ExecContext(ctx, "UPDATE outbox SET attempts=? WHERE id=?", j.Attempts, j.ID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE destinations SET next_allowed=? WHERE id=?", now.Add(interval).UnixMilli(), dest); err != nil {
		return nil, err
	}
	return &j, tx.Commit()
}

func (s *Store) Sent(ctx context.Context, j Job, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE outbox SET status='sent', sent_at=?,last_error='' WHERE id=?", now.UnixMilli(), j.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE destinations SET last_sent=? WHERE id=?", now.UnixMilli(), j.Destination); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Failed(ctx context.Context, j Job, next time.Time, code string, permanent bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE outbox SET next_attempt=?,last_error=? WHERE id=?", next.UnixMilli(), code, j.ID); err != nil {
		return err
	}
	// Apply server cooling hints to ALL jobs on the destination, including alerts.
	if _, err = tx.ExecContext(ctx, "UPDATE destinations SET next_allowed=MAX(next_allowed,?),blocked=? WHERE id=?", next.UnixMilli(), permanent, j.Destination); err != nil {
		return err
	}
	return tx.Commit()
}

func queueAlert(ctx context.Context, tx *sql.Tx, key, text, exclude string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO outbox(dedup_key,destination,kind,body,created_at)
SELECT ? || ':' || id, id, 'alert', ?, ? FROM destinations WHERE enabled=1 AND blocked=0 AND id<>?`, key, text, now.UnixMilli(), exclude)
	return err
}

func (s *Store) Fault(ctx context.Context, key, description, exclude string, now time.Time, threshold int, cooldown time.Duration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO faults(key,failures) VALUES(?,1) ON CONFLICT(key) DO UPDATE SET failures=failures+1`, key); err != nil {
		return err
	}
	var failures int
	var alerted bool
	var last int64
	if err = tx.QueryRowContext(ctx, "SELECT failures,alerted,last_alert FROM faults WHERE key=?", key).Scan(&failures, &alerted, &last); err != nil {
		return err
	}
	if failures >= threshold && (!alerted || now.UnixMilli()-last >= cooldown.Milliseconds()) {
		body := "Tibo 监控告警\n" + description + "\n时间：" + now.UTC().Format(time.RFC3339) + "\n请检查服务日志和配置；未完成的发送任务会保留。"
		if err = queueAlert(ctx, tx, fmt.Sprintf("fault:%s:%d", key, now.UnixNano()), body, exclude, now); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE faults SET alerted=1,last_alert=? WHERE key=?", now.UnixMilli(), key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Recover(ctx context.Context, key, description string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var alerted bool
	err = tx.QueryRowContext(ctx, "SELECT alerted FROM faults WHERE key=?", key).Scan(&alerted)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if alerted {
		if err = queueAlert(ctx, tx, fmt.Sprintf("recovery:%s:%d", key, now.UnixNano()), "Tibo 监控恢复\n"+description+"\n时间："+now.UTC().Format(time.RFC3339), "", now); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM faults WHERE key=?", key); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Status(ctx context.Context) (Status, error) {
	var st Status
	var err error
	st.Scan, err = s.State(ctx)
	if err != nil {
		return st, err
	}
	for _, q := range []struct {
		query string
		ptr   *int
	}{
		{"SELECT COUNT(*) FROM posts", &st.Posts},
		{"SELECT COUNT(*) FROM outbox WHERE status='pending'", &st.Pending},
		{"SELECT COUNT(*) FROM outbox WHERE status='sent'", &st.Sent},
	} {
		if err = s.db.QueryRowContext(ctx, q.query).Scan(q.ptr); err != nil {
			return st, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.id,d.enabled,d.blocked,d.last_sent,(SELECT COUNT(*) FROM outbox o WHERE o.destination=d.id AND o.status='pending') FROM destinations d ORDER BY d.id`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var d DestinationStatus
		if err = rows.Scan(&d.ID, &d.Enabled, &d.Blocked, &d.LastSent, &d.Pending); err != nil {
			return st, err
		}
		st.Destinations = append(st.Destinations, d)
	}
	// Cursors are internal pagination tokens, not useful for status output.
	st.Scan.Cursor = ""
	return st, rows.Err()
}
