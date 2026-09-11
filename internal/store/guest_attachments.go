package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type GuestAttachment struct {
	ID            string     `json:"id"`
	GuestSubject  string     `json:"guestSubject"`
	DeviceID      string     `json:"deviceId"`
	Name          string     `json:"name"`
	RelativePath  string     `json:"relativePath"`
	Source        string     `json:"source"`
	MediaType     string     `json:"mediaType"`
	SizeBytes     int64      `json:"sizeBytes"`
	SHA256        string     `json:"sha256"`
	StoragePath   string     `json:"-"`
	ExtractStatus string     `json:"extractStatus"`
	ExtractedText string     `json:"-"`
	CreatedAt     time.Time  `json:"createdAt"`
	DeletedAt     *time.Time `json:"deletedAt,omitempty"`
}

func scanGuestAttachment(row pgx.Row) (GuestAttachment, error) {
	var a GuestAttachment
	err := row.Scan(&a.ID, &a.GuestSubject, &a.DeviceID, &a.Name, &a.RelativePath, &a.Source, &a.MediaType, &a.SizeBytes, &a.SHA256, &a.StoragePath, &a.ExtractStatus, &a.ExtractedText, &a.CreatedAt, &a.DeletedAt)
	return a, err
}

const guestAttachmentColumns = `id,guest_subject,device_id,name,relative_path,source,media_type,size_bytes,sha256,storage_path,extract_status,extracted_text,created_at,deleted_at`

func (s *Store) CreateGuestAttachment(ctx context.Context, a GuestAttachment) (GuestAttachment, error) {
	return scanGuestAttachment(s.DB.QueryRow(ctx, `INSERT INTO guest_attachments(id,guest_subject,device_id,name,relative_path,source,media_type,size_bytes,sha256,storage_path,extract_status,extracted_text)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING `+guestAttachmentColumns,
		a.ID, a.GuestSubject, a.DeviceID, a.Name, a.RelativePath, a.Source, a.MediaType, a.SizeBytes, a.SHA256, a.StoragePath, a.ExtractStatus, a.ExtractedText))
}

func (s *Store) GuestAttachment(ctx context.Context, guestSubject, deviceID, id string) (GuestAttachment, error) {
	return scanGuestAttachment(s.DB.QueryRow(ctx, `SELECT `+guestAttachmentColumns+` FROM guest_attachments WHERE id=$1 AND guest_subject=$2 AND device_id=$3 AND deleted_at IS NULL`, id, guestSubject, deviceID))
}
func (s *Store) UpdateGuestAttachmentExtraction(ctx context.Context, guestSubject, deviceID, id, status, text string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE guest_attachments SET extract_status=$4,extracted_text=$5 WHERE id=$1 AND guest_subject=$2 AND device_id=$3 AND deleted_at IS NULL`, id, guestSubject, deviceID, status, text)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (s *Store) GuestAttachments(ctx context.Context, guestSubject, deviceID string, ids []string) ([]GuestAttachment, error) {
	if len(ids) == 0 {
		return []GuestAttachment{}, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT `+guestAttachmentColumns+` FROM guest_attachments WHERE guest_subject=$1 AND device_id=$2 AND id=ANY($3) AND deleted_at IS NULL ORDER BY created_at ASC`, guestSubject, deviceID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GuestAttachment{}
	for rows.Next() {
		a, err := scanGuestAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GuestAttachmentList(ctx context.Context, guestSubject, deviceID string, limit int) ([]GuestAttachment, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.DB.Query(ctx, `SELECT `+guestAttachmentColumns+` FROM guest_attachments WHERE guest_subject=$1 AND device_id=$2 AND deleted_at IS NULL ORDER BY created_at DESC LIMIT $3`, guestSubject, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GuestAttachment{}
	for rows.Next() {
		a, err := scanGuestAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteGuestAttachment(ctx context.Context, guestSubject, deviceID, id string) (GuestAttachment, error) {
	return scanGuestAttachment(s.DB.QueryRow(ctx, `UPDATE guest_attachments SET deleted_at=now() WHERE id=$1 AND guest_subject=$2 AND device_id=$3 AND deleted_at IS NULL RETURNING `+guestAttachmentColumns, id, guestSubject, deviceID))
}

func (s *Store) GuestAttachmentUsage(ctx context.Context, guestSubject string) (files int64, bytes int64, err error) {
	err = s.DB.QueryRow(ctx, `SELECT count(*),COALESCE(sum(size_bytes),0) FROM guest_attachments WHERE guest_subject=$1 AND deleted_at IS NULL`, guestSubject).Scan(&files, &bytes)
	return
}

func (s *Store) GuestUploadsSince(ctx context.Context, guestSubject string, since time.Time) (int64, error) {
	var n int64
	err := s.DB.QueryRow(ctx, `SELECT count(*) FROM guest_attachments WHERE guest_subject=$1 AND created_at >= $2`, guestSubject, since).Scan(&n)
	return n, err
}

func (s *Store) ExpireGuestAttachments(ctx context.Context, before time.Time, limit int) ([]GuestAttachment, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.DB.Query(ctx, `UPDATE guest_attachments SET deleted_at=now() WHERE id IN (SELECT id FROM guest_attachments WHERE deleted_at IS NULL AND created_at < $1 ORDER BY created_at ASC LIMIT $2) RETURNING `+guestAttachmentColumns, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GuestAttachment{}
	for rows.Next() {
		a, err := scanGuestAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GuestAttachmentsForAdmin(ctx context.Context, guestSubject, deviceID string, limit int) ([]GuestAttachment, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT ` + guestAttachmentColumns + ` FROM guest_attachments WHERE guest_subject=$1 AND deleted_at IS NULL`
	args := []any{guestSubject}
	if deviceID != "" {
		q += ` AND device_id=$2`
		args = append(args, deviceID)
	}
	q += ` ORDER BY created_at DESC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, limit)
	rows, err := s.DB.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GuestAttachment{}
	for rows.Next() {
		a, err := scanGuestAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
