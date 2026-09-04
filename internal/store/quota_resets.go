package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type QuotaResetGrant struct {
	ID              int64     `json:"id"`
	UserSubject     string    `json:"userSubject"`
	TotalResets     int       `json:"totalResets"`
	RemainingResets int       `json:"remainingResets"`
	ExpiresAt       time.Time `json:"expiresAt"`
	Note            string    `json:"note"`
	CreatedBy       string    `json:"createdBy"`
	CreatedAt       time.Time `json:"createdAt"`
}

type QuotaResetStatus struct {
	Available   int               `json:"available"`
	NextExpiry  *time.Time        `json:"nextExpiry,omitempty"`
	LastResetAt *time.Time        `json:"lastResetAt,omitempty"`
	Grants      []QuotaResetGrant `json:"grants"`
}

type QuotaResetEvent struct {
	ID           int64     `json:"id"`
	UserSubject  string    `json:"userSubject"`
	GrantID      *int64    `json:"grantId,omitempty"`
	ResetKind    string    `json:"resetKind"`
	ActorSubject string    `json:"actorSubject"`
	ResetAt      time.Time `json:"resetAt"`
	Note         string    `json:"note"`
}

func scanResetGrant(row pgx.Row) (QuotaResetGrant, error) {
	var x QuotaResetGrant
	err := row.Scan(&x.ID, &x.UserSubject, &x.TotalResets, &x.RemainingResets, &x.ExpiresAt, &x.Note, &x.CreatedBy, &x.CreatedAt)
	return x, err
}

func (s *Store) GrantQuotaResets(ctx context.Context, actor, subject string, count int, expiresAt time.Time, note string) (QuotaResetGrant, error) {
	if count <= 0 || count > 1000 {
		return QuotaResetGrant{}, errors.New("reset count must be between 1 and 1000")
	}
	if !expiresAt.After(time.Now().UTC()) {
		return QuotaResetGrant{}, errors.New("reset expiry must be in the future")
	}
	x, err := scanResetGrant(s.DB.QueryRow(ctx, `INSERT INTO quota_reset_grants(user_subject,total_resets,remaining_resets,expires_at,note,created_by) SELECT subject,$2,$2,$3,$4,$5 FROM app_users WHERE subject=$1 RETURNING id,user_subject,total_resets,remaining_resets,expires_at,note,created_by,created_at`, subject, count, expiresAt.UTC(), note, actor))
	if err != nil {
		return QuotaResetGrant{}, err
	}
	newJSON, _ := json.Marshal(x)
	_, _ = s.DB.Exec(ctx, `INSERT INTO access_audit_log(actor_subject,action,target_type,target_id,new_value) VALUES($1,'quota.reset.grant','user',$2,$3)`, actor, subject, newJSON)
	return x, nil
}

func (s *Store) QuotaResetStatus(ctx context.Context, subject string) (QuotaResetStatus, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,user_subject,total_resets,remaining_resets,expires_at,note,created_by,created_at FROM quota_reset_grants WHERE user_subject=$1 AND remaining_resets>0 AND expires_at>now() ORDER BY expires_at ASC,id ASC`, subject)
	if err != nil {
		return QuotaResetStatus{}, err
	}
	defer rows.Close()
	out := QuotaResetStatus{Grants: []QuotaResetGrant{}}
	for rows.Next() {
		var x QuotaResetGrant
		if err := rows.Scan(&x.ID, &x.UserSubject, &x.TotalResets, &x.RemainingResets, &x.ExpiresAt, &x.Note, &x.CreatedBy, &x.CreatedAt); err != nil {
			return QuotaResetStatus{}, err
		}
		out.Grants = append(out.Grants, x)
		out.Available += x.RemainingResets
		if out.NextExpiry == nil || x.ExpiresAt.Before(*out.NextExpiry) {
			t := x.ExpiresAt
			out.NextExpiry = &t
		}
	}
	if err := rows.Err(); err != nil {
		return QuotaResetStatus{}, err
	}
	var last *time.Time
	if err := s.DB.QueryRow(ctx, `SELECT max(reset_at) FROM quota_reset_events WHERE user_subject=$1`, subject).Scan(&last); err != nil {
		return QuotaResetStatus{}, err
	}
	out.LastResetAt = last
	return out, nil
}

func (s *Store) LatestQuotaResetAt(ctx context.Context, subject string) (*time.Time, error) {
	var last *time.Time
	err := s.DB.QueryRow(ctx, `SELECT max(reset_at) FROM quota_reset_events WHERE user_subject=$1`, subject).Scan(&last)
	return last, err
}

func (s *Store) AdminResetQuota(ctx context.Context, actor, subject, note string) (QuotaResetEvent, error) {
	var x QuotaResetEvent
	err := s.DB.QueryRow(ctx, `INSERT INTO quota_reset_events(user_subject,reset_kind,actor_subject,note) SELECT subject,'admin',$2,$3 FROM app_users WHERE subject=$1 RETURNING id,user_subject,grant_id,reset_kind,actor_subject,reset_at,note`, subject, actor, note).Scan(&x.ID, &x.UserSubject, &x.GrantID, &x.ResetKind, &x.ActorSubject, &x.ResetAt, &x.Note)
	if err != nil {
		return QuotaResetEvent{}, err
	}
	newJSON, _ := json.Marshal(x)
	_, _ = s.DB.Exec(ctx, `INSERT INTO access_audit_log(actor_subject,action,target_type,target_id,new_value) VALUES($1,'quota.reset.admin','user',$2,$3)`, actor, subject, newJSON)
	return x, nil
}

func (s *Store) UseQuotaResetCredit(ctx context.Context, subject string) (QuotaResetEvent, error) {
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return QuotaResetEvent{}, err
	}
	defer tx.Rollback(ctx)
	var grantID int64
	var before int
	err = tx.QueryRow(ctx, `SELECT id,remaining_resets FROM quota_reset_grants WHERE user_subject=$1 AND remaining_resets>0 AND expires_at>now() ORDER BY expires_at ASC,id ASC FOR UPDATE SKIP LOCKED LIMIT 1`, subject).Scan(&grantID, &before)
	if err != nil {
		return QuotaResetEvent{}, err
	}
	var after int
	if err = tx.QueryRow(ctx, `UPDATE quota_reset_grants SET remaining_resets=remaining_resets-1 WHERE id=$1 AND remaining_resets>0 RETURNING remaining_resets`, grantID).Scan(&after); err != nil {
		return QuotaResetEvent{}, err
	}
	var x QuotaResetEvent
	err = tx.QueryRow(ctx, `INSERT INTO quota_reset_events(user_subject,grant_id,reset_kind,actor_subject,note) VALUES($1,$2,'credit',$1,'User redeemed quota reset credit') RETURNING id,user_subject,grant_id,reset_kind,actor_subject,reset_at,note`, subject, grantID).Scan(&x.ID, &x.UserSubject, &x.GrantID, &x.ResetKind, &x.ActorSubject, &x.ResetAt, &x.Note)
	if err != nil {
		return QuotaResetEvent{}, err
	}
	oldJSON, _ := json.Marshal(map[string]any{"grantId": grantID, "remainingResets": before})
	newJSON, _ := json.Marshal(map[string]any{"grantId": grantID, "remainingResets": after, "resetAt": x.ResetAt})
	if _, err = tx.Exec(ctx, `INSERT INTO access_audit_log(actor_subject,action,target_type,target_id,old_value,new_value) VALUES($1,'quota.reset.consume','user',$1,$2,$3)`, subject, oldJSON, newJSON); err != nil {
		return QuotaResetEvent{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return QuotaResetEvent{}, err
	}
	return x, nil
}
