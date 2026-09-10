package store

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type ChatSession struct {
	ID           string    `json:"id"`
	OwnerSubject string    `json:"-"`
	Title        string    `json:"title"`
	ModelAlias   string    `json:"modelAlias"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type ChatMessage struct {
	ID            int64     `json:"id"`
	SessionID     string    `json:"sessionId"`
	Role          string    `json:"role"`
	Content       string    `json:"content"`
	AttachmentIDs []string  `json:"attachmentIds"`
	RunID         *string   `json:"runId,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

func scanChatSession(row pgx.Row) (ChatSession, error) {
	var x ChatSession
	err := row.Scan(&x.ID, &x.OwnerSubject, &x.Title, &x.ModelAlias, &x.CreatedAt, &x.UpdatedAt)
	return x, err
}

func (s *Store) CreateChatSession(ctx context.Context, x ChatSession) (ChatSession, error) {
	return scanChatSession(s.DB.QueryRow(ctx, `INSERT INTO chat_sessions(id,owner_subject,title,model_alias) VALUES($1,$2,$3,$4) RETURNING id,owner_subject,title,model_alias,created_at,updated_at`, x.ID, x.OwnerSubject, x.Title, x.ModelAlias))
}

func (s *Store) ChatSessions(ctx context.Context, owner string, limit int) ([]ChatSession, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.DB.Query(ctx, `SELECT id,owner_subject,title,model_alias,created_at,updated_at FROM chat_sessions WHERE owner_subject=$1 ORDER BY updated_at DESC LIMIT $2`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatSession{}
	for rows.Next() {
		var x ChatSession
		if err := rows.Scan(&x.ID, &x.OwnerSubject, &x.Title, &x.ModelAlias, &x.CreatedAt, &x.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) SearchChatSessions(ctx context.Context, owner, query string, limit int) ([]ChatSession, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return s.ChatSessions(ctx, owner, limit)
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.DB.Query(ctx, `SELECT s.id,s.owner_subject,s.title,s.model_alias,s.created_at,s.updated_at
		FROM chat_sessions s
		WHERE s.owner_subject=$1
		  AND (strpos(lower(s.title), lower($2)) > 0
		       OR EXISTS (SELECT 1 FROM chat_messages m WHERE m.session_id=s.id AND strpos(lower(m.content), lower($2)) > 0))
		ORDER BY s.updated_at DESC
		LIMIT $3`, owner, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatSession{}
	for rows.Next() {
		var x ChatSession
		if err := rows.Scan(&x.ID, &x.OwnerSubject, &x.Title, &x.ModelAlias, &x.CreatedAt, &x.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) ChatSession(ctx context.Context, owner, id string) (ChatSession, error) {
	return scanChatSession(s.DB.QueryRow(ctx, `SELECT id,owner_subject,title,model_alias,created_at,updated_at FROM chat_sessions WHERE id=$1 AND owner_subject=$2`, id, owner))
}

func (s *Store) UpdateChatSession(ctx context.Context, owner, id, title, modelAlias string) (ChatSession, error) {
	return scanChatSession(s.DB.QueryRow(ctx, `UPDATE chat_sessions SET title=CASE WHEN $3='' THEN title ELSE $3 END,model_alias=CASE WHEN $4='' THEN model_alias ELSE $4 END,updated_at=now() WHERE id=$1 AND owner_subject=$2 RETURNING id,owner_subject,title,model_alias,created_at,updated_at`, id, owner, title, modelAlias))
}

func (s *Store) DeleteChatSession(ctx context.Context, owner, id string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM chat_sessions WHERE id=$1 AND owner_subject=$2`, id, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (s *Store) ChatMessages(ctx context.Context, owner, sessionID string) ([]ChatMessage, error) {
	rows, err := s.DB.Query(ctx, `SELECT m.id,m.session_id,m.role,m.content,m.attachment_ids,m.run_id,m.created_at FROM chat_messages m JOIN chat_sessions s ON s.id=m.session_id WHERE m.session_id=$1 AND s.owner_subject=$2 ORDER BY m.created_at ASC,m.id ASC`, sessionID, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatMessage{}
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.AttachmentIDs, &m.RunID, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) AddChatMessage(ctx context.Context, owner, sessionID, role, content string, attachmentIDs []string) (ChatMessage, error) {
	if attachmentIDs == nil {
		attachmentIDs = []string{}
	}
	var m ChatMessage
	err := s.DB.QueryRow(ctx, `WITH owned AS (SELECT id FROM chat_sessions WHERE id=$1 AND owner_subject=$2), ins AS (INSERT INTO chat_messages(session_id,role,content,attachment_ids) SELECT id,$3,$4,$5 FROM owned RETURNING id,session_id,role,content,attachment_ids,run_id,created_at), touch AS (UPDATE chat_sessions SET updated_at=now() WHERE id IN (SELECT id FROM owned)) SELECT id,session_id,role,content,attachment_ids,run_id,created_at FROM ins`, sessionID, owner, role, content, attachmentIDs).Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.AttachmentIDs, &m.RunID, &m.CreatedAt)
	return m, err
}

func (s *Store) AddChatMessageForRun(ctx context.Context, owner, sessionID, role, content string, attachmentIDs []string, runID string) (ChatMessage, error) {
	if attachmentIDs == nil {
		attachmentIDs = []string{}
	}
	var m ChatMessage
	err := s.DB.QueryRow(ctx, `WITH owned AS (SELECT id FROM chat_sessions WHERE id=$1 AND owner_subject=$2), ins AS (INSERT INTO chat_messages(session_id,role,content,attachment_ids,run_id) SELECT id,$3,$4,$5,$6 FROM owned RETURNING id,session_id,role,content,attachment_ids,run_id,created_at), touch AS (UPDATE chat_sessions SET updated_at=now() WHERE id IN (SELECT id FROM owned)) SELECT id,session_id,role,content,attachment_ids,run_id,created_at FROM ins`, sessionID, owner, role, content, attachmentIDs, runID).Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.AttachmentIDs, &m.RunID, &m.CreatedAt)
	return m, err
}

func (s *Store) EditUserChatMessageAndTruncate(ctx context.Context, owner, sessionID string, messageID int64, content string) (ChatMessage, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return ChatMessage{}, err
	}
	defer tx.Rollback(ctx)
	var m ChatMessage
	err = tx.QueryRow(ctx, `UPDATE chat_messages m SET content=$4 WHERE m.id=$3 AND m.session_id=$1 AND m.role='user' AND EXISTS(SELECT 1 FROM chat_sessions s WHERE s.id=$1 AND s.owner_subject=$2) RETURNING m.id,m.session_id,m.role,m.content,m.attachment_ids,m.run_id,m.created_at`, sessionID, owner, messageID, content).Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.AttachmentIDs, &m.RunID, &m.CreatedAt)
	if err != nil {
		return ChatMessage{}, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM chat_messages WHERE session_id=$1 AND id>$2`, sessionID, messageID); err != nil {
		return ChatMessage{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE chat_sessions SET updated_at=now() WHERE id=$1`, sessionID); err != nil {
		return ChatMessage{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ChatMessage{}, err
	}
	return m, nil
}
