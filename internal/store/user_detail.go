package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type UsageBucket struct {
	Label        string `json:"label"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	TotalTokens  int64  `json:"totalTokens"`
	Requests     int64  `json:"requests"`
}

type CapabilityUsage struct {
	Kind        string     `json:"kind"`
	Name        string     `json:"name"`
	Requests    int64      `json:"requests"`
	TotalTokens int64      `json:"totalTokens"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
}

type OAuthConnection struct {
	Provider    string    `json:"provider"`
	Email       string    `json:"email"`
	Scopes      []string  `json:"scopes"`
	ConnectedAt time.Time `json:"connectedAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type ActivityRow struct {
	RequestID    string          `json:"requestId"`
	APIKeyID     *string         `json:"apiKeyId,omitempty"`
	ModelAlias   string          `json:"modelAlias"`
	Workload     string          `json:"workload"`
	InputTokens  int64           `json:"inputTokens"`
	OutputTokens int64           `json:"outputTokens"`
	TotalTokens  int64           `json:"totalTokens"`
	Status       string          `json:"status"`
	Metadata     json.RawMessage `json:"metadata"`
	StartedAt    time.Time       `json:"startedAt"`
	CompletedAt  *time.Time      `json:"completedAt,omitempty"`
}

func (s *Store) APIKeysForUser(ctx context.Context, subject string) ([]APIKey, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,owner_subject,name,key_prefix,scopes,status,created_by,created_at,expires_at,last_used_at,revoked_at FROM api_keys WHERE owner_subject=$1 ORDER BY created_at DESC`, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) OAuthConnectionsForUser(ctx context.Context, subject string) ([]OAuthConnection, error) {
	rows, err := s.DB.Query(ctx, `SELECT provider,email,scopes,connected_at,updated_at FROM oauth_integrations WHERE subject=$1 ORDER BY updated_at DESC`, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OAuthConnection{}
	for rows.Next() {
		var x OAuthConnection
		if err := rows.Scan(&x.Provider, &x.Email, &x.Scopes, &x.ConnectedAt, &x.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) UsageTotalsForUser(ctx context.Context, subject string, since time.Time) (Usage, int64, error) {
	var u Usage
	var requests int64
	err := s.DB.QueryRow(ctx, `SELECT COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0),COALESCE(sum(total_tokens),0),count(*) FROM usage_ledger WHERE user_subject=$1 AND status='completed' AND started_at >= $2`, subject, since).Scan(&u.InputTokens, &u.OutputTokens, &u.TotalTokens, &requests)
	return u, requests, err
}

func (s *Store) UsageSeriesForUser(ctx context.Context, subject, period, timezone string, since time.Time) ([]UsageBucket, error) {
	unit := "day"
	format := "YYYY-MM-DD"
	switch period {
	case "day":
		unit, format = "hour", "HH24:00"
	case "month":
		unit, format = "day", "YYYY-MM-DD"
	case "year":
		unit, format = "month", "YYYY-MM"
	default:
		return nil, fmt.Errorf("invalid usage period")
	}
	q := fmt.Sprintf(`SELECT to_char(date_trunc('%s', started_at AT TIME ZONE $3),'%s') label,COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0),COALESCE(sum(total_tokens),0),count(*) FROM usage_ledger WHERE user_subject=$1 AND status='completed' AND started_at >= $2 GROUP BY 1 ORDER BY 1`, unit, format)
	rows, err := s.DB.Query(ctx, q, subject, since, timezone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageBucket{}
	for rows.Next() {
		var x UsageBucket
		if err := rows.Scan(&x.Label, &x.InputTokens, &x.OutputTokens, &x.TotalTokens, &x.Requests); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) CapabilitiesForUser(ctx context.Context, subject string) ([]CapabilityUsage, error) {
	rows, err := s.DB.Query(ctx, `
		WITH caps AS (
			SELECT request_id,total_tokens,started_at,'tool'::text kind,jsonb_array_elements_text(COALESCE(metadata->'toolsUsed',metadata->'tools','[]'::jsonb)) name FROM usage_ledger WHERE user_subject=$1
			UNION ALL SELECT request_id,total_tokens,started_at,'skill',jsonb_array_elements_text(COALESCE(metadata->'skills','[]'::jsonb)) FROM usage_ledger WHERE user_subject=$1
			UNION ALL SELECT request_id,total_tokens,started_at,'mcp',jsonb_array_elements_text(COALESCE(metadata->'mcpServers','[]'::jsonb)) FROM usage_ledger WHERE user_subject=$1
			UNION ALL SELECT request_id,total_tokens,started_at,'connector',jsonb_array_elements_text(COALESCE(metadata->'connectors','[]'::jsonb)) FROM usage_ledger WHERE user_subject=$1
		)
		SELECT kind,name,count(DISTINCT request_id),COALESCE(sum(total_tokens),0),max(started_at) FROM caps WHERE name<>'' GROUP BY kind,name ORDER BY kind,count(DISTINCT request_id) DESC,name`, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CapabilityUsage{}
	for rows.Next() {
		var x CapabilityUsage
		if err := rows.Scan(&x.Kind, &x.Name, &x.Requests, &x.TotalTokens, &x.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) RecentActivityForUser(ctx context.Context, subject string, limit int) ([]ActivityRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.Query(ctx, `SELECT request_id,api_key_id,model_alias,workload,input_tokens,output_tokens,total_tokens,status,metadata,started_at,completed_at FROM usage_ledger WHERE user_subject=$1 ORDER BY started_at DESC LIMIT $2`, subject, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ActivityRow{}
	for rows.Next() {
		var x ActivityRow
		if err := rows.Scan(&x.RequestID, &x.APIKeyID, &x.ModelAlias, &x.Workload, &x.InputTokens, &x.OutputTokens, &x.TotalTokens, &x.Status, &x.Metadata, &x.StartedAt, &x.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) MergeUsageMetadata(ctx context.Context, requestID string, patch any) error {
	b, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `UPDATE usage_ledger SET metadata=metadata || $2::jsonb WHERE request_id=$1`, requestID, b)
	return err
}
