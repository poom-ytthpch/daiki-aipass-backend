package store

import (
	"context"
	"encoding/json"
	"time"
)

// RecentResearchSourceMetadata returns only the public research-source array
// persisted in usage metadata. It intentionally excludes user identity,
// messages, prompts and generated answers so callers can reuse public search
// provenance without carrying conversation content across users.
func (s *Store) RecentResearchSourceMetadata(ctx context.Context, since time.Time, limit int) ([]json.RawMessage, error) {
	if s == nil || s.DB == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.DB.Query(ctx, `
		SELECT COALESCE(metadata->'research'->'sources','[]'::jsonb)
		FROM usage_ledger
		WHERE status='completed'
		  AND started_at >= $1
		  AND jsonb_typeof(metadata->'research'->'sources')='array'
		  AND jsonb_array_length(metadata->'research'->'sources') > 0
		ORDER BY started_at DESC
		LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]json.RawMessage, 0, limit)
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, rows.Err()
}
