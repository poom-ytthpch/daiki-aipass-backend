package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type Attachment struct {
	ID            string     `json:"id"`
	OwnerSubject  string     `json:"ownerSubject"`
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

func scanAttachment(row pgx.Row) (Attachment, error) {
	var a Attachment
	err := row.Scan(&a.ID, &a.OwnerSubject, &a.Name, &a.RelativePath, &a.Source, &a.MediaType, &a.SizeBytes, &a.SHA256, &a.StoragePath, &a.ExtractStatus, &a.ExtractedText, &a.CreatedAt, &a.DeletedAt)
	return a, err
}

func (s *Store) CreateAttachment(ctx context.Context, a Attachment) (Attachment, error) {
	return scanAttachment(s.DB.QueryRow(ctx, `INSERT INTO attachments(id,owner_subject,name,relative_path,source,media_type,size_bytes,sha256,storage_path,extract_status,extracted_text)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id,owner_subject,name,relative_path,source,media_type,size_bytes,sha256,storage_path,extract_status,extracted_text,created_at,deleted_at`,
		a.ID, a.OwnerSubject, a.Name, a.RelativePath, a.Source, a.MediaType, a.SizeBytes, a.SHA256, a.StoragePath, a.ExtractStatus, a.ExtractedText))
}

func (s *Store) Attachment(ctx context.Context, owner, id string) (Attachment, error) {
	return scanAttachment(s.DB.QueryRow(ctx, `SELECT id,owner_subject,name,relative_path,source,media_type,size_bytes,sha256,storage_path,extract_status,extracted_text,created_at,deleted_at
		FROM attachments WHERE id=$1 AND owner_subject=$2 AND deleted_at IS NULL`, id, owner))
}

func (s *Store) Attachments(ctx context.Context, owner string, ids []string) ([]Attachment, error) {
	if len(ids) == 0 {
		return []Attachment{}, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT id,owner_subject,name,relative_path,source,media_type,size_bytes,sha256,storage_path,extract_status,extracted_text,created_at,deleted_at
		FROM attachments WHERE owner_subject=$1 AND id=ANY($2) AND deleted_at IS NULL ORDER BY created_at ASC`, owner, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) UserAttachments(ctx context.Context, owner string, limit int) ([]Attachment, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.DB.Query(ctx, `SELECT id,owner_subject,name,relative_path,source,media_type,size_bytes,sha256,storage_path,extract_status,extracted_text,created_at,deleted_at
		FROM attachments WHERE owner_subject=$1 AND deleted_at IS NULL ORDER BY created_at DESC LIMIT $2`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAttachment(ctx context.Context, owner, id string) (Attachment, error) {
	return scanAttachment(s.DB.QueryRow(ctx, `UPDATE attachments SET deleted_at=now() WHERE id=$1 AND owner_subject=$2 AND deleted_at IS NULL
		RETURNING id,owner_subject,name,relative_path,source,media_type,size_bytes,sha256,storage_path,extract_status,extracted_text,created_at,deleted_at`, id, owner))
}

type AttachmentSearchResult struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	RelativePath  string `json:"relativePath"`
	MediaType     string `json:"mediaType"`
	ExtractStatus string `json:"extractStatus"`
	Snippet       string `json:"snippet"`
}

func (s *Store) SearchAttachments(ctx context.Context, owner, query string, limit int) ([]AttachmentSearchResult, error) {
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	rows, err := s.DB.Query(ctx, `SELECT id,name,relative_path,media_type,extract_status,
		LEFT(extracted_text,12000)
		FROM attachments
		WHERE owner_subject=$1 AND deleted_at IS NULL
		AND ($2='' OR name ILIKE '%'||$2||'%' OR relative_path ILIKE '%'||$2||'%' OR extracted_text ILIKE '%'||$2||'%')
		ORDER BY CASE WHEN name ILIKE '%'||$2||'%' OR relative_path ILIKE '%'||$2||'%' THEN 0 ELSE 1 END, created_at DESC
		LIMIT $3`, owner, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AttachmentSearchResult{}
	for rows.Next() {
		var x AttachmentSearchResult
		if err := rows.Scan(&x.ID, &x.Name, &x.RelativePath, &x.MediaType, &x.ExtractStatus, &x.Snippet); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
