package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type GuestAccessPolicy struct {
	Enabled                  bool       `json:"enabled"`
	TokenLimit               int64      `json:"tokenLimit"`
	IntervalKind             string     `json:"intervalKind"`
	IntervalSeconds          *int64     `json:"intervalSeconds,omitempty"`
	RequestsPerHour          int        `json:"requestsPerHour"`
	MinIntervalSeconds       int        `json:"minIntervalSeconds"`
	MaxCompletionTokens      int        `json:"maxCompletionTokens"`
	FastModel                string     `json:"fastModel"`
	AllowUploads             bool       `json:"allowUploads"`
	AllowImageGeneration     bool       `json:"allowImageGeneration"`
	AllowFileGeneration      bool       `json:"allowFileGeneration"`
	MaxUploadBytes           int64      `json:"maxUploadBytes"`
	MaxUploadsPerHour        int        `json:"maxUploadsPerHour"`
	MaxStoredFiles           int        `json:"maxStoredFiles"`
	MaxStoredBytes           int64      `json:"maxStoredBytes"`
	AttachmentRetentionHours int        `json:"attachmentRetentionHours"`
	ImageGenerationsPerDay   int        `json:"imageGenerationsPerDay"`
	FileGenerationsPerDay    int        `json:"fileGenerationsPerDay"`
	MaxGeneratedFileBytes    int64      `json:"maxGeneratedFileBytes"`
	UpdatedBy                string     `json:"updatedBy,omitempty"`
	UpdatedAt                *time.Time `json:"updatedAt,omitempty"`
}

type GuestDeviceUsage struct {
	GuestSubject string     `json:"guestSubject"`
	DeviceID     string     `json:"deviceId"`
	DeviceName   string     `json:"deviceName,omitempty"`
	Requests     int64      `json:"requests"`
	InputTokens  int64      `json:"inputTokens"`
	OutputTokens int64      `json:"outputTokens"`
	TotalTokens  int64      `json:"totalTokens"`
	LastSeenAt   time.Time  `json:"lastSeenAt"`
	LastUsedAt   *time.Time `json:"lastUsedAt,omitempty"`
}

func DefaultGuestAccessPolicy() GuestAccessPolicy {
	return GuestAccessPolicy{
		Enabled: true, TokenLimit: 4000, IntervalKind: "day", RequestsPerHour: 6,
		MinIntervalSeconds: 45, MaxCompletionTokens: 384, FastModel: "fast",
		AllowUploads: true, AllowImageGeneration: true, AllowFileGeneration: true,
		MaxUploadBytes: 10 << 20, MaxUploadsPerHour: 10, MaxStoredFiles: 20,
		MaxStoredBytes: 50 << 20, AttachmentRetentionHours: 24,
		ImageGenerationsPerDay: 3, FileGenerationsPerDay: 5, MaxGeneratedFileBytes: 1 << 20,
	}
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
	if p.MaxUploadBytes <= 0 || p.MaxUploadBytes > 100<<20 {
		return p, errors.New("maxUploadBytes must be between 1 and 104857600")
	}
	if p.MaxUploadsPerHour <= 0 || p.MaxUploadsPerHour > 1000 {
		return p, errors.New("maxUploadsPerHour must be between 1 and 1000")
	}
	if p.MaxStoredFiles <= 0 || p.MaxStoredFiles > 1000 {
		return p, errors.New("maxStoredFiles must be between 1 and 1000")
	}
	if p.MaxStoredBytes <= 0 || p.MaxStoredBytes > 2<<30 {
		return p, errors.New("maxStoredBytes must be between 1 and 2147483648")
	}
	if p.AttachmentRetentionHours <= 0 || p.AttachmentRetentionHours > 24*30 {
		return p, errors.New("attachmentRetentionHours must be between 1 and 720")
	}
	if p.ImageGenerationsPerDay < 0 || p.ImageGenerationsPerDay > 1000 {
		return p, errors.New("imageGenerationsPerDay must be between 0 and 1000")
	}
	if p.FileGenerationsPerDay < 0 || p.FileGenerationsPerDay > 1000 {
		return p, errors.New("fileGenerationsPerDay must be between 0 and 1000")
	}
	if p.MaxGeneratedFileBytes <= 0 || p.MaxGeneratedFileBytes > 25<<20 {
		return p, errors.New("maxGeneratedFileBytes must be between 1 and 26214400")
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

const guestPolicyColumns = `enabled,token_limit,interval_kind,interval_seconds,requests_per_hour,min_interval_seconds,max_completion_tokens,fast_model,allow_uploads,allow_image_generation,allow_file_generation,max_upload_bytes,max_uploads_per_hour,max_stored_files,max_stored_bytes,attachment_retention_hours,image_generations_per_day,file_generations_per_day,max_generated_file_bytes,COALESCE(updated_by,''),updated_at`

func scanGuestPolicy(row pgx.Row) (GuestAccessPolicy, error) {
	p := DefaultGuestAccessPolicy()
	err := row.Scan(&p.Enabled, &p.TokenLimit, &p.IntervalKind, &p.IntervalSeconds, &p.RequestsPerHour, &p.MinIntervalSeconds, &p.MaxCompletionTokens, &p.FastModel, &p.AllowUploads, &p.AllowImageGeneration, &p.AllowFileGeneration, &p.MaxUploadBytes, &p.MaxUploadsPerHour, &p.MaxStoredFiles, &p.MaxStoredBytes, &p.AttachmentRetentionHours, &p.ImageGenerationsPerDay, &p.FileGenerationsPerDay, &p.MaxGeneratedFileBytes, &p.UpdatedBy, &p.UpdatedAt)
	return p, err
}

func (s *Store) GuestAccessPolicy(ctx context.Context) (GuestAccessPolicy, bool, error) {
	p, err := scanGuestPolicy(s.DB.QueryRow(ctx, `SELECT `+guestPolicyColumns+` FROM guest_access_policy WHERE singleton=TRUE`))
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultGuestAccessPolicy(), false, nil
	}
	return p, err == nil, err
}

func (s *Store) UpsertGuestAccessPolicy(ctx context.Context, actor string, p GuestAccessPolicy) (GuestAccessPolicy, error) {
	p, err := validateGuestAccessPolicy(p)
	if err != nil {
		return GuestAccessPolicy{}, err
	}
	q := `INSERT INTO guest_access_policy(singleton,enabled,token_limit,interval_kind,interval_seconds,requests_per_hour,min_interval_seconds,max_completion_tokens,fast_model,allow_uploads,allow_image_generation,allow_file_generation,max_upload_bytes,max_uploads_per_hour,max_stored_files,max_stored_bytes,attachment_retention_hours,image_generations_per_day,file_generations_per_day,max_generated_file_bytes,updated_by)
	VALUES(TRUE,$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
	ON CONFLICT(singleton) DO UPDATE SET enabled=EXCLUDED.enabled,token_limit=EXCLUDED.token_limit,interval_kind=EXCLUDED.interval_kind,interval_seconds=EXCLUDED.interval_seconds,requests_per_hour=EXCLUDED.requests_per_hour,min_interval_seconds=EXCLUDED.min_interval_seconds,max_completion_tokens=EXCLUDED.max_completion_tokens,fast_model=EXCLUDED.fast_model,allow_uploads=EXCLUDED.allow_uploads,allow_image_generation=EXCLUDED.allow_image_generation,allow_file_generation=EXCLUDED.allow_file_generation,max_upload_bytes=EXCLUDED.max_upload_bytes,max_uploads_per_hour=EXCLUDED.max_uploads_per_hour,max_stored_files=EXCLUDED.max_stored_files,max_stored_bytes=EXCLUDED.max_stored_bytes,attachment_retention_hours=EXCLUDED.attachment_retention_hours,image_generations_per_day=EXCLUDED.image_generations_per_day,file_generations_per_day=EXCLUDED.file_generations_per_day,max_generated_file_bytes=EXCLUDED.max_generated_file_bytes,updated_by=EXCLUDED.updated_by,updated_at=now()
	RETURNING ` + guestPolicyColumns
	out, err := scanGuestPolicy(s.DB.QueryRow(ctx, q, p.Enabled, p.TokenLimit, p.IntervalKind, p.IntervalSeconds, p.RequestsPerHour, p.MinIntervalSeconds, p.MaxCompletionTokens, p.FastModel, p.AllowUploads, p.AllowImageGeneration, p.AllowFileGeneration, p.MaxUploadBytes, p.MaxUploadsPerHour, p.MaxStoredFiles, p.MaxStoredBytes, p.AttachmentRetentionHours, p.ImageGenerationsPerDay, p.FileGenerationsPerDay, p.MaxGeneratedFileBytes, actor))
	if err != nil {
		return GuestAccessPolicy{}, err
	}
	_, _ = s.DB.Exec(ctx, `INSERT INTO access_audit_log(actor_subject,action,target_type,target_id,new_value) VALUES($1,'guest.policy.upsert','guest','default',jsonb_build_object('enabled',$2,'tokenLimit',$3,'requestsPerHour',$4,'minIntervalSeconds',$5,'maxCompletionTokens',$6,'allowUploads',$7,'allowImageGeneration',$8,'allowFileGeneration',$9,'maxUploadBytes',$10,'maxUploadsPerHour',$11,'maxStoredFiles',$12,'maxStoredBytes',$13,'attachmentRetentionHours',$14,'imageGenerationsPerDay',$15,'fileGenerationsPerDay',$16,'maxGeneratedFileBytes',$17))`, actor, out.Enabled, out.TokenLimit, out.RequestsPerHour, out.MinIntervalSeconds, out.MaxCompletionTokens, out.AllowUploads, out.AllowImageGeneration, out.AllowFileGeneration, out.MaxUploadBytes, out.MaxUploadsPerHour, out.MaxStoredFiles, out.MaxStoredBytes, out.AttachmentRetentionHours, out.ImageGenerationsPerDay, out.FileGenerationsPerDay, out.MaxGeneratedFileBytes)
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

func (s *Store) UpsertGuestDevice(ctx context.Context, guestSubject, deviceID, deviceName, userAgentHash string) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO guest_devices(guest_subject,device_id,device_name,user_agent_hash) VALUES($1,$2,$3,$4)
	ON CONFLICT(guest_subject,device_id) DO UPDATE SET device_name=CASE WHEN EXCLUDED.device_name<>'' THEN EXCLUDED.device_name ELSE guest_devices.device_name END,user_agent_hash=EXCLUDED.user_agent_hash,last_seen_at=now()`, guestSubject, deviceID, deviceName, userAgentHash)
	return err
}

func (s *Store) GuestUsageBreakdown(ctx context.Context, limit int) ([]GuestDeviceUsage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.DB.Query(ctx, `SELECT d.guest_subject,d.device_id,d.device_name,d.last_seen_at,
		COALESCE(count(l.request_id) FILTER (WHERE l.status='completed'),0),
		COALESCE(sum(l.input_tokens) FILTER (WHERE l.status='completed'),0),
		COALESCE(sum(l.output_tokens) FILTER (WHERE l.status='completed'),0),
		COALESCE(sum(l.total_tokens) FILTER (WHERE l.status='completed'),0),
		max(l.started_at) FILTER (WHERE l.status='completed')
	FROM guest_devices d
	LEFT JOIN usage_ledger l ON l.subject_id=d.guest_subject AND COALESCE(l.metadata->>'guestDeviceId','')=d.device_id
	GROUP BY d.guest_subject,d.device_id,d.device_name,d.last_seen_at
	ORDER BY d.last_seen_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GuestDeviceUsage{}
	for rows.Next() {
		var x GuestDeviceUsage
		if err := rows.Scan(&x.GuestSubject, &x.DeviceID, &x.DeviceName, &x.LastSeenAt, &x.Requests, &x.InputTokens, &x.OutputTokens, &x.TotalTokens, &x.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) GuestLastReset(ctx context.Context, guestSubject string) (*time.Time, error) {
	var t *time.Time
	err := s.DB.QueryRow(ctx, `SELECT max(reset_at) FROM guest_quota_reset_events WHERE guest_subject=$1`, guestSubject).Scan(&t)
	return t, err
}

func (s *Store) ResetGuestQuota(ctx context.Context, guestSubject, actor, note string) (time.Time, error) {
	var t time.Time
	err := s.DB.QueryRow(ctx, `INSERT INTO guest_quota_reset_events(guest_subject,actor_subject,note) VALUES($1,$2,$3) RETURNING reset_at`, guestSubject, actor, note).Scan(&t)
	if err == nil {
		_, _ = s.DB.Exec(ctx, `INSERT INTO access_audit_log(actor_subject,action,target_type,target_id,new_value) VALUES($1,'guest.quota.reset','guest',$2,jsonb_build_object('note',$3,'resetAt',$4))`, actor, guestSubject, note, t)
	}
	return t, err
}
