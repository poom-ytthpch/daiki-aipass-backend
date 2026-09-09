package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type GuestAccessPolicy struct {
	Enabled             bool       `json:"enabled"`
	TokenLimit          int64      `json:"tokenLimit"`
	IntervalKind        string     `json:"intervalKind"`
	IntervalSeconds     *int64     `json:"intervalSeconds,omitempty"`
	RequestsPerHour     int        `json:"requestsPerHour"`
	MinIntervalSeconds  int        `json:"minIntervalSeconds"`
	MaxCompletionTokens int        `json:"maxCompletionTokens"`
	FastModel           string     `json:"fastModel"`
	UpdatedBy           string     `json:"updatedBy,omitempty"`
	UpdatedAt           *time.Time `json:"updatedAt,omitempty"`
}

func DefaultGuestAccessPolicy() GuestAccessPolicy {
	return GuestAccessPolicy{Enabled: true, TokenLimit: 4000, IntervalKind: "day", RequestsPerHour: 6, MinIntervalSeconds: 45, MaxCompletionTokens: 384, FastModel: "fast"}
}

func validateGuestAccessPolicy(p GuestAccessPolicy) (GuestAccessPolicy, error) {
	if p.TokenLimit < 0 {
		return p, errors.New("tokenLimit must be >= 0")
	}
	if p.RequestsPerHour <= 0 || p.RequestsPerHour > 10000 {
		return p, errors.New("requestsPerHour must be between 1 and 10000")
	}
	if p.MinIntervalSeconds < 0 || p.MinIntervalSeconds > 3600 {
		return p, errors.New("minIntervalSeconds must be between 0 and 3600")
	}
	if p.MaxCompletionTokens <= 0 || p.MaxCompletionTokens > 8192 {
		return p, errors.New("maxCompletionTokens must be between 1 and 8192")
	}
	if p.IntervalKind == "" {
		p.IntervalKind = "day"
	}
	switch p.IntervalKind {
	case "hour", "day", "week", "month", "lifetime":
	case "rolling", "custom":
		if p.IntervalSeconds == nil || *p.IntervalSeconds <= 0 {
			return p, errors.New("rolling/custom interval requires intervalSeconds")
		}
	default:
		return p, errors.New("invalid intervalKind")
	}
	p.FastModel = strings.TrimSpace(p.FastModel)
	if p.FastModel == "" {
		p.FastModel = "fast"
	}
	return p, nil
}

func (s *Store) GuestAccessPolicy(ctx context.Context) (GuestAccessPolicy, bool, error) {
	p := DefaultGuestAccessPolicy()
	if s == nil || s.DB == nil {
		return p, false, errors.New("store unavailable")
	}
	err := s.DB.QueryRow(ctx, `SELECT enabled,token_limit,interval_kind,interval_seconds,requests_per_hour,min_interval_seconds,max_completion_tokens,fast_model,COALESCE(updated_by,''),updated_at FROM guest_access_policy WHERE singleton=TRUE`).Scan(&p.Enabled, &p.TokenLimit, &p.IntervalKind, &p.IntervalSeconds, &p.RequestsPerHour, &p.MinIntervalSeconds, &p.MaxCompletionTokens, &p.FastModel, &p.UpdatedBy, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (s *Store) UpsertGuestAccessPolicy(ctx context.Context, actor string, p GuestAccessPolicy) (GuestAccessPolicy, error) {
	p, err := validateGuestAccessPolicy(p)
	if err != nil {
		return GuestAccessPolicy{}, err
	}
	row := s.DB.QueryRow(ctx, `INSERT INTO guest_access_policy(singleton,enabled,token_limit,interval_kind,interval_seconds,requests_per_hour,min_interval_seconds,max_completion_tokens,fast_model,updated_by) VALUES(TRUE,$1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(singleton) DO UPDATE SET enabled=EXCLUDED.enabled,token_limit=EXCLUDED.token_limit,interval_kind=EXCLUDED.interval_kind,interval_seconds=EXCLUDED.interval_seconds,requests_per_hour=EXCLUDED.requests_per_hour,min_interval_seconds=EXCLUDED.min_interval_seconds,max_completion_tokens=EXCLUDED.max_completion_tokens,fast_model=EXCLUDED.fast_model,updated_by=EXCLUDED.updated_by,updated_at=now() RETURNING enabled,token_limit,interval_kind,interval_seconds,requests_per_hour,min_interval_seconds,max_completion_tokens,fast_model,COALESCE(updated_by,''),updated_at`, p.Enabled, p.TokenLimit, p.IntervalKind, p.IntervalSeconds, p.RequestsPerHour, p.MinIntervalSeconds, p.MaxCompletionTokens, p.FastModel, actor)
	var out GuestAccessPolicy
	if err := row.Scan(&out.Enabled, &out.TokenLimit, &out.IntervalKind, &out.IntervalSeconds, &out.RequestsPerHour, &out.MinIntervalSeconds, &out.MaxCompletionTokens, &out.FastModel, &out.UpdatedBy, &out.UpdatedAt); err != nil {
		return GuestAccessPolicy{}, err
	}
	_, _ = s.DB.Exec(ctx, `INSERT INTO access_audit_log(actor_subject,action,target_type,target_id,new_value) VALUES($1,'guest.policy.upsert','guest','default',jsonb_build_object('enabled',$2,'tokenLimit',$3,'requestsPerHour',$4,'minIntervalSeconds',$5,'maxCompletionTokens',$6,'fastModel',$7))`, actor, out.Enabled, out.TokenLimit, out.RequestsPerHour, out.MinIntervalSeconds, out.MaxCompletionTokens, out.FastModel)
	return out, nil
}

func (s *Store) GuestUsageSummary(ctx context.Context, guestSubject string, since time.Time) (Usage, error) {
	var u Usage
	err := s.DB.QueryRow(ctx, `SELECT COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0),COALESCE(sum(total_tokens),0) FROM usage_ledger WHERE subject_type='user' AND subject_id=$1 AND status='completed' AND started_at >= $2`, guestSubject, since).Scan(&u.InputTokens, &u.OutputTokens, &u.TotalTokens)
	return u, err
}

func (s *Store) GuestUsageTotals(ctx context.Context) (Usage, int64, error) {
	var u Usage
	var requests int64
	err := s.DB.QueryRow(ctx, `SELECT COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0),COALESCE(sum(total_tokens),0),count(*) FROM usage_ledger WHERE subject_type='user' AND subject_id LIKE 'guest:%' AND status='completed'`).Scan(&u.InputTokens, &u.OutputTokens, &u.TotalTokens, &requests)
	return u, requests, err
}
