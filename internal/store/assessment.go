package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"tibo-monitor/internal/model"
)

// Post returns the unabridged evidence, before card context snippets are applied.
// A debug post outside the collection window may not exist in this table.
func (s *Store) Post(ctx context.Context, id string) (*model.Post, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT data FROM posts WHERE id=?", id).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	var p model.Post
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// RecentPosts is relative to the post being judged, NOT delivery time. Even
// during catch-up/retries, later announcements cannot leak into older judgments.
// Equal timestamps are ordered by decimal ID without an overflowing integer cast.
func (s *Store) RecentPosts(ctx context.Context, p model.Post, limit int, window time.Duration) ([]model.Post, error) {
	if limit < 1 || limit > 10 || window <= 0 {
		return nil, errors.New("invalid reset history bounds")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM posts
WHERE json_extract(data,'$.author_id')=? AND id<>? AND created_at>=?
AND (created_at<? OR (created_at=? AND (length(id)<? OR (length(id)=? AND id<?))))
ORDER BY created_at DESC, length(id) DESC, id DESC LIMIT ?`,
		p.AuthorID, p.ID, p.CreatedAt.Add(-window).UnixMilli(), p.CreatedAt.UnixMilli(), p.CreatedAt.UnixMilli(),
		len(p.ID), len(p.ID), p.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	posts := make([]model.Post, 0, limit)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var previous model.Post
		if err := json.Unmarshal([]byte(raw), &previous); err != nil {
			return nil, err
		}
		posts = append(posts, previous)
	}
	return posts, rows.Err()
}

func (s *Store) ResetAssessment(ctx context.Context, key string) (*model.ResetAssessment, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT data FROM reset_assessments WHERE key=?", key).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	var a model.ResetAssessment
	if err := json.Unmarshal([]byte(raw), &a); err != nil || !a.Valid() {
		return nil, errors.New("invalid stored reset assessment")
	}
	return &a, nil
}

// Only genuine successful judgments are cached. Credentials and upstream error
// bodies never enter the business database. Keys distinguish normal posts from
// each debug batch, so debug replay always makes a fresh inference request.
func (s *Store) SaveResetAssessment(ctx context.Context, key string, a model.ResetAssessment) error {
	if !a.Valid() {
		return errors.New("invalid reset assessment")
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO reset_assessments(key,data) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET data=excluded.data", key, string(raw))
	return err
}
