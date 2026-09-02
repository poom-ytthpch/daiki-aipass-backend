package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

type Store struct{ DB *pgxpool.Pool }

type User struct {
	Subject      string     `json:"subject"`
	Email        string     `json:"email"`
	DisplayName  string     `json:"displayName"`
	AuthProvider string     `json:"authProvider"`
	Status       string     `json:"status"`
	Roles        []string   `json:"roles"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	LastLoginAt  *time.Time `json:"lastLoginAt,omitempty"`
	ApprovedBy   *string    `json:"approvedBy,omitempty"`
	ApprovedAt   *time.Time `json:"approvedAt,omitempty"`
}

type Policy struct {
	ID              int64           `json:"id"`
	ScopeType       string          `json:"scopeType"`
	ScopeID         string          `json:"scopeId"`
	QuotaMode       string          `json:"quotaMode"`
	TokenLimit      *int64          `json:"tokenLimit,omitempty"`
	IntervalKind    string          `json:"intervalKind"`
	IntervalSeconds *int64          `json:"intervalSeconds,omitempty"`
	Priority        int             `json:"priority"`
	AllowedModels   json.RawMessage `json:"allowedModels"`
	EffectiveFrom   time.Time       `json:"effectiveFrom"`
	ExpiresAt       *time.Time      `json:"expiresAt,omitempty"`
}

type Usage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	TotalTokens  int64 `json:"totalTokens"`
}

type AdminStats struct {
	Users       int64 `json:"users"`
	Pending     int64 `json:"pending"`
	Approved    int64 `json:"approved"`
	Suspended   int64 `json:"suspended"`
	Rejected    int64 `json:"rejected"`
	TotalTokens int64 `json:"totalTokens"`
	Requests    int64 `json:"requests"`
}

type AdminUserUsageRow struct {
	Subject      string     `json:"subject"`
	Email        string     `json:"email"`
	DisplayName  string     `json:"displayName"`
	Status       string     `json:"status"`
	LastLoginAt  *time.Time `json:"lastLoginAt,omitempty"`
	InputTokens  int64      `json:"inputTokens"`
	OutputTokens int64      `json:"outputTokens"`
	TotalTokens  int64      `json:"totalTokens"`
	Requests     int64      `json:"requests"`
}

func New(db *pgxpool.Pool) *Store { return &Store{DB: db} }

func (s *Store) AdminStats(ctx context.Context) (AdminStats, error) {
	var out AdminStats
	err := s.DB.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE status='pending'),count(*) FILTER (WHERE status='approved'),count(*) FILTER (WHERE status='suspended'),count(*) FILTER (WHERE status='rejected') FROM app_users`).Scan(&out.Users, &out.Pending, &out.Approved, &out.Suspended, &out.Rejected)
	if err != nil {
		return out, err
	}
	err = s.DB.QueryRow(ctx, `SELECT COALESCE(sum(total_tokens),0),count(*) FROM usage_ledger WHERE status='completed'`).Scan(&out.TotalTokens, &out.Requests)
	return out, err
}

func (s *Store) AdminUserUsage(ctx context.Context) ([]AdminUserUsageRow, error) {
	rows, err := s.DB.Query(ctx, `SELECT u.subject,u.email,u.display_name,u.status,u.last_login_at,COALESCE(sum(l.input_tokens) FILTER (WHERE l.status='completed'),0),COALESCE(sum(l.output_tokens) FILTER (WHERE l.status='completed'),0),COALESCE(sum(l.total_tokens) FILTER (WHERE l.status='completed'),0),count(l.request_id) FILTER (WHERE l.status='completed') FROM app_users u LEFT JOIN usage_ledger l ON l.user_subject=u.subject GROUP BY u.subject,u.email,u.display_name,u.status,u.last_login_at ORDER BY COALESCE(sum(l.total_tokens) FILTER (WHERE l.status='completed'),0) DESC,u.created_at DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminUserUsageRow{}
	for rows.Next() {
		var x AdminUserUsageRow
		if err := rows.Scan(&x.Subject, &x.Email, &x.DisplayName, &x.Status, &x.LastLoginAt, &x.InputTokens, &x.OutputTokens, &x.TotalTokens, &x.Requests); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.DB == nil {
		return errors.New("store unavailable")
	}
	_, err := s.DB.Exec(ctx, schemaSQL)
	return err
}

func (s *Store) UpsertLogin(ctx context.Context, subject, email, name, provider string, roles []string) (User, error) {
	if provider == "" {
		provider = "keycloak"
	}
	row := s.DB.QueryRow(ctx, `INSERT INTO app_users(subject,email,display_name,auth_provider,roles,last_login_at) VALUES($1,$2,$3,$4,$5,now()) ON CONFLICT(subject) DO UPDATE SET email=EXCLUDED.email,display_name=EXCLUDED.display_name,auth_provider=EXCLUDED.auth_provider,roles=EXCLUDED.roles,last_login_at=now(),updated_at=now() RETURNING subject,email,display_name,auth_provider,status,roles,created_at,updated_at,last_login_at,approved_by,approved_at`, subject, email, name, provider, roles)
	return scanUser(row)
}
func (s *Store) User(ctx context.Context, subject string) (User, error) {
	return scanUser(s.DB.QueryRow(ctx, `SELECT subject,email,display_name,auth_provider,status,roles,created_at,updated_at,last_login_at,approved_by,approved_at FROM app_users WHERE subject=$1`, subject))
}
func (s *Store) Users(ctx context.Context, status string) ([]User, error) {
	q := `SELECT subject,email,display_name,auth_provider,status,roles,created_at,updated_at,last_login_at,approved_by,approved_at FROM app_users`
	args := []any{}
	if status != "" {
		q += ` WHERE status=$1`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT 500`
	rows, err := s.DB.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.Subject, &u.Email, &u.DisplayName, &u.AuthProvider, &u.Status, &u.Roles, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt, &u.ApprovedBy, &u.ApprovedAt)
	return u, err
}
func (s *Store) SetUserStatus(ctx context.Context, actor, subject, status string) (User, error) {
	if status != "pending" && status != "approved" && status != "suspended" && status != "rejected" {
		return User{}, fmt.Errorf("invalid user status %q", status)
	}
	old, err := s.User(ctx, subject)
	if err != nil {
		return User{}, err
	}
	approved := status == "approved"
	u, err := scanUser(s.DB.QueryRow(ctx, `UPDATE app_users SET status=$2,updated_at=now(),approved_by=CASE WHEN $3 THEN $4 ELSE approved_by END,approved_at=CASE WHEN $3 THEN now() ELSE approved_at END WHERE subject=$1 RETURNING subject,email,display_name,auth_provider,status,roles,created_at,updated_at,last_login_at,approved_by,approved_at`, subject, status, approved, actor))
	if err != nil {
		return User{}, err
	}
	oldJSON, _ := json.Marshal(map[string]any{"status": old.Status})
	newJSON, _ := json.Marshal(map[string]any{"status": u.Status})
	_, _ = s.DB.Exec(ctx, `INSERT INTO access_audit_log(actor_subject,action,target_type,target_id,old_value,new_value) VALUES($1,'user.status.update','user',$2,$3,$4)`, actor, subject, oldJSON, newJSON)
	return u, nil
}
func (s *Store) PolicyForUser(ctx context.Context, subject string, roles []string) (Policy, bool, error) {
	row := s.DB.QueryRow(ctx, `SELECT id,scope_type,scope_id,quota_mode,token_limit,interval_kind,interval_seconds,priority,allowed_models,effective_from,expires_at FROM entitlement_policies WHERE effective_from<=now() AND (expires_at IS NULL OR expires_at>now()) AND ((scope_type='user' AND scope_id=$1) OR (scope_type='role' AND scope_id=ANY($2)) OR (scope_type='system' AND scope_id='default')) ORDER BY CASE scope_type WHEN 'user' THEN 3 WHEN 'role' THEN 2 ELSE 1 END DESC,priority DESC,id DESC LIMIT 1`, subject, roles)
	var p Policy
	err := row.Scan(&p.ID, &p.ScopeType, &p.ScopeID, &p.QuotaMode, &p.TokenLimit, &p.IntervalKind, &p.IntervalSeconds, &p.Priority, &p.AllowedModels, &p.EffectiveFrom, &p.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Policy{QuotaMode: "unlimited", IntervalKind: "lifetime"}, false, nil
	}
	return p, err == nil, err
}
func (s *Store) UpsertUserPolicy(ctx context.Context, actor, subject string, p Policy) (Policy, error) {
	if p.QuotaMode != "unlimited" && p.QuotaMode != "limited" {
		return Policy{}, errors.New("quotaMode must be unlimited or limited")
	}
	if p.QuotaMode == "limited" && (p.TokenLimit == nil || *p.TokenLimit < 0) {
		return Policy{}, errors.New("limited quota requires tokenLimit")
	}
	if p.IntervalKind == "" {
		p.IntervalKind = "lifetime"
	}
	if p.IntervalKind != "hour" && p.IntervalKind != "day" && p.IntervalKind != "week" && p.IntervalKind != "month" && p.IntervalKind != "rolling" && p.IntervalKind != "custom" && p.IntervalKind != "lifetime" {
		return Policy{}, errors.New("invalid intervalKind")
	}
	if (p.IntervalKind == "rolling" || p.IntervalKind == "custom") && (p.IntervalSeconds == nil || *p.IntervalSeconds <= 0) {
		return Policy{}, errors.New("rolling/custom interval requires intervalSeconds")
	}
	if len(p.AllowedModels) == 0 {
		p.AllowedModels = json.RawMessage(`[]`)
	}
	row := s.DB.QueryRow(ctx, `INSERT INTO entitlement_policies(scope_type,scope_id,quota_mode,token_limit,interval_kind,interval_seconds,priority,allowed_models,created_by) VALUES('user',$1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(scope_type,scope_id) DO UPDATE SET quota_mode=EXCLUDED.quota_mode,token_limit=EXCLUDED.token_limit,interval_kind=EXCLUDED.interval_kind,interval_seconds=EXCLUDED.interval_seconds,priority=EXCLUDED.priority,allowed_models=EXCLUDED.allowed_models,updated_at=now() RETURNING id,scope_type,scope_id,quota_mode,token_limit,interval_kind,interval_seconds,priority,allowed_models,effective_from,expires_at`, subject, p.QuotaMode, p.TokenLimit, p.IntervalKind, p.IntervalSeconds, p.Priority, p.AllowedModels, actor)
	var out Policy
	if err := row.Scan(&out.ID, &out.ScopeType, &out.ScopeID, &out.QuotaMode, &out.TokenLimit, &out.IntervalKind, &out.IntervalSeconds, &out.Priority, &out.AllowedModels, &out.EffectiveFrom, &out.ExpiresAt); err != nil {
		return Policy{}, err
	}
	newJSON, _ := json.Marshal(out)
	_, _ = s.DB.Exec(ctx, `INSERT INTO access_audit_log(actor_subject,action,target_type,target_id,new_value) VALUES($1,'quota.policy.upsert','user',$2,$3)`, actor, subject, newJSON)
	return out, nil
}
func (s *Store) StartUsage(ctx context.Context, requestID, subject, modelAlias, workload string, reserved int64) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO usage_ledger(request_id,subject_type,subject_id,user_subject,model_alias,workload,reserved_tokens,status) VALUES($1,'user',$2,$2,$3,$4,$5,'reserved') ON CONFLICT(request_id) DO NOTHING`, requestID, subject, modelAlias, workload, reserved)
	return err
}
func (s *Store) FinishUsage(ctx context.Context, requestID, status string, u Usage) error {
	if status != "completed" && status != "cancelled" && status != "failed" {
		return errors.New("invalid usage status")
	}
	_, err := s.DB.Exec(ctx, `UPDATE usage_ledger SET input_tokens=$2,output_tokens=$3,total_tokens=$4,status=$5,completed_at=now() WHERE request_id=$1`, requestID, u.InputTokens, u.OutputTokens, u.TotalTokens, status)
	return err
}
func (s *Store) UsageSummary(ctx context.Context, subject string, since time.Time) (Usage, error) {
	var u Usage
	err := s.DB.QueryRow(ctx, `SELECT COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0),COALESCE(sum(total_tokens),0) FROM usage_ledger WHERE subject_type='user' AND subject_id=$1 AND status='completed' AND started_at >= $2`, subject, since).Scan(&u.InputTokens, &u.OutputTokens, &u.TotalTokens)
	return u, err
}

type APIKey struct {
	ID           string     `json:"id"`
	OwnerSubject string     `json:"ownerSubject"`
	Name         string     `json:"name"`
	KeyPrefix    string     `json:"keyPrefix"`
	Scopes       []string   `json:"scopes"`
	Status       string     `json:"status"`
	CreatedBy    string     `json:"createdBy"`
	CreatedAt    time.Time  `json:"createdAt"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	LastUsedAt   *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt    *time.Time `json:"revokedAt,omitempty"`
}

func (s *Store) CreateAPIKey(ctx context.Context, actor string, key APIKey, hash string) (APIKey, error) {
	row := s.DB.QueryRow(ctx, `INSERT INTO api_keys(id,owner_subject,name,key_prefix,key_hash,scopes,created_by,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id,owner_subject,name,key_prefix,scopes,status,created_by,created_at,expires_at,last_used_at,revoked_at`, key.ID, key.OwnerSubject, key.Name, key.KeyPrefix, hash, key.Scopes, actor, key.ExpiresAt)
	return scanAPIKey(row)
}
func (s *Store) APIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,owner_subject,name,key_prefix,scopes,status,created_by,created_at,expires_at,last_used_at,revoked_at FROM api_keys ORDER BY created_at DESC LIMIT 500`)
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
func scanAPIKey(row pgx.Row) (APIKey, error) {
	var k APIKey
	err := row.Scan(&k.ID, &k.OwnerSubject, &k.Name, &k.KeyPrefix, &k.Scopes, &k.Status, &k.CreatedBy, &k.CreatedAt, &k.ExpiresAt, &k.LastUsedAt, &k.RevokedAt)
	return k, err
}
func (s *Store) AuthenticateAPIKey(ctx context.Context, hash string) (APIKey, User, error) {
	row := s.DB.QueryRow(ctx, `SELECT k.id,k.owner_subject,k.name,k.key_prefix,k.scopes,k.status,k.created_by,k.created_at,k.expires_at,k.last_used_at,k.revoked_at,u.subject,u.email,u.display_name,u.auth_provider,u.status,u.roles,u.created_at,u.updated_at,u.last_login_at,u.approved_by,u.approved_at FROM api_keys k JOIN app_users u ON u.subject=k.owner_subject WHERE k.key_hash=$1 AND k.status='active' AND (k.expires_at IS NULL OR k.expires_at>now())`, hash)
	var k APIKey
	var u User
	err := row.Scan(&k.ID, &k.OwnerSubject, &k.Name, &k.KeyPrefix, &k.Scopes, &k.Status, &k.CreatedBy, &k.CreatedAt, &k.ExpiresAt, &k.LastUsedAt, &k.RevokedAt, &u.Subject, &u.Email, &u.DisplayName, &u.AuthProvider, &u.Status, &u.Roles, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt, &u.ApprovedBy, &u.ApprovedAt)
	if err != nil {
		return APIKey{}, User{}, err
	}
	_, _ = s.DB.Exec(ctx, `UPDATE api_keys SET last_used_at=now() WHERE id=$1`, k.ID)
	return k, u, nil
}
func (s *Store) RevokeAPIKey(ctx context.Context, actor, id string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE api_keys SET status='revoked',revoked_at=now() WHERE id=$1 AND status='active'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	_, _ = s.DB.Exec(ctx, `INSERT INTO access_audit_log(actor_subject,action,target_type,target_id,new_value) VALUES($1,'api_key.revoke','api_key',$2,'{"status":"revoked"}'::jsonb)`, actor, id)
	return nil
}

func (s *Store) PolicyForPrincipal(ctx context.Context, subject, apiKeyID string, roles []string) (Policy, bool, error) {
	row := s.DB.QueryRow(ctx, `SELECT id,scope_type,scope_id,quota_mode,token_limit,interval_kind,interval_seconds,priority,allowed_models,effective_from,expires_at FROM entitlement_policies WHERE effective_from<=now() AND (expires_at IS NULL OR expires_at>now()) AND ((scope_type='api_key' AND scope_id=$2 AND $2<>'') OR (scope_type='user' AND scope_id=$1) OR (scope_type='role' AND scope_id=ANY($3)) OR (scope_type='system' AND scope_id='default')) ORDER BY CASE scope_type WHEN 'api_key' THEN 4 WHEN 'user' THEN 3 WHEN 'role' THEN 2 ELSE 1 END DESC,priority DESC,id DESC LIMIT 1`, subject, apiKeyID, roles)
	var p Policy
	err := row.Scan(&p.ID, &p.ScopeType, &p.ScopeID, &p.QuotaMode, &p.TokenLimit, &p.IntervalKind, &p.IntervalSeconds, &p.Priority, &p.AllowedModels, &p.EffectiveFrom, &p.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Policy{QuotaMode: "unlimited", IntervalKind: "lifetime"}, false, nil
	}
	return p, err == nil, err
}

func (s *Store) UpsertPolicy(ctx context.Context, actor, scopeType, scopeID string, p Policy) (Policy, error) {
	if scopeType != "user" && scopeType != "api_key" && scopeType != "role" && scopeType != "workspace" && scopeType != "project" && scopeType != "plan" && scopeType != "system" {
		return Policy{}, errors.New("invalid scopeType")
	}
	if p.QuotaMode != "unlimited" && p.QuotaMode != "limited" {
		return Policy{}, errors.New("quotaMode must be unlimited or limited")
	}
	if p.QuotaMode == "limited" && (p.TokenLimit == nil || *p.TokenLimit < 0) {
		return Policy{}, errors.New("limited quota requires tokenLimit")
	}
	if p.IntervalKind == "" {
		p.IntervalKind = "lifetime"
	}
	if p.IntervalKind != "hour" && p.IntervalKind != "day" && p.IntervalKind != "week" && p.IntervalKind != "month" && p.IntervalKind != "rolling" && p.IntervalKind != "custom" && p.IntervalKind != "lifetime" {
		return Policy{}, errors.New("invalid intervalKind")
	}
	if (p.IntervalKind == "rolling" || p.IntervalKind == "custom") && (p.IntervalSeconds == nil || *p.IntervalSeconds <= 0) {
		return Policy{}, errors.New("rolling/custom interval requires intervalSeconds")
	}
	if len(p.AllowedModels) == 0 {
		p.AllowedModels = json.RawMessage(`[]`)
	}
	row := s.DB.QueryRow(ctx, `INSERT INTO entitlement_policies(scope_type,scope_id,quota_mode,token_limit,interval_kind,interval_seconds,priority,allowed_models,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(scope_type,scope_id) DO UPDATE SET quota_mode=EXCLUDED.quota_mode,token_limit=EXCLUDED.token_limit,interval_kind=EXCLUDED.interval_kind,interval_seconds=EXCLUDED.interval_seconds,priority=EXCLUDED.priority,allowed_models=EXCLUDED.allowed_models,updated_at=now() RETURNING id,scope_type,scope_id,quota_mode,token_limit,interval_kind,interval_seconds,priority,allowed_models,effective_from,expires_at`, scopeType, scopeID, p.QuotaMode, p.TokenLimit, p.IntervalKind, p.IntervalSeconds, p.Priority, p.AllowedModels, actor)
	var out Policy
	if err := row.Scan(&out.ID, &out.ScopeType, &out.ScopeID, &out.QuotaMode, &out.TokenLimit, &out.IntervalKind, &out.IntervalSeconds, &out.Priority, &out.AllowedModels, &out.EffectiveFrom, &out.ExpiresAt); err != nil {
		return Policy{}, err
	}
	newJSON, _ := json.Marshal(out)
	_, _ = s.DB.Exec(ctx, `INSERT INTO access_audit_log(actor_subject,action,target_type,target_id,new_value) VALUES($1,'quota.policy.upsert',$2,$3,$4)`, actor, scopeType, scopeID, newJSON)
	return out, nil
}
func (s *Store) UsageSummaryForUser(ctx context.Context, subject string, since time.Time) (Usage, error) {
	var u Usage
	err := s.DB.QueryRow(ctx, `SELECT COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0),COALESCE(sum(total_tokens),0) FROM usage_ledger WHERE user_subject=$1 AND status='completed' AND started_at >= $2`, subject, since).Scan(&u.InputTokens, &u.OutputTokens, &u.TotalTokens)
	return u, err
}
func (s *Store) UsageSummaryForAPIKey(ctx context.Context, id string, since time.Time) (Usage, error) {
	var u Usage
	err := s.DB.QueryRow(ctx, `SELECT COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0),COALESCE(sum(total_tokens),0) FROM usage_ledger WHERE subject_type='api_key' AND subject_id=$1 AND status='completed' AND started_at >= $2`, id, since).Scan(&u.InputTokens, &u.OutputTokens, &u.TotalTokens)
	return u, err
}
func (s *Store) StartUsageForPrincipal(ctx context.Context, requestID, subject, apiKeyID, modelAlias, workload string, reserved int64) error {
	subjectType := "user"
	subjectID := subject
	if apiKeyID != "" {
		subjectType = "api_key"
		subjectID = apiKeyID
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO usage_ledger(request_id,subject_type,subject_id,user_subject,api_key_id,model_alias,workload,reserved_tokens,status) VALUES($1,$2,$3,$4,NULLIF($5,''),$6,$7,$8,'reserved') ON CONFLICT(request_id) DO NOTHING`, requestID, subjectType, subjectID, subject, apiKeyID, modelAlias, workload, reserved)
	return err
}
