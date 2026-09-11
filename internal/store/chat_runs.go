package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type ChatRun struct {
	ID            string          `json:"id"`
	SessionID     string          `json:"sessionId"`
	OwnerSubject  string          `json:"-"`
	Status        string          `json:"status"`
	ResearchMode  string          `json:"researchMode"`
	ThinkingMode  string          `json:"thinkingMode"`
	CommandMode   string          `json:"commandMode,omitempty"`
	CommandSkills []string        `json:"commandSkills,omitempty"`
	RequestID     string          `json:"requestId,omitempty"`
	Content       string          `json:"content"`
	Error         string          `json:"error,omitempty"`
	Activity      json.RawMessage `json:"activity"`
	CreatedAt     time.Time       `json:"createdAt"`
	StartedAt     *time.Time      `json:"startedAt,omitempty"`
	CompletedAt   *time.Time      `json:"completedAt,omitempty"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

func scanChatRun(row pgx.Row) (ChatRun, error) {
	var x ChatRun
	err := row.Scan(&x.ID, &x.SessionID, &x.OwnerSubject, &x.Status, &x.ResearchMode, &x.ThinkingMode, &x.CommandMode, &x.CommandSkills, &x.RequestID, &x.Content, &x.Error, &x.Activity, &x.CreatedAt, &x.StartedAt, &x.CompletedAt, &x.UpdatedAt)
	return x, err
}

const chatRunCols = `id,session_id,owner_subject,status,research_mode,thinking_mode,command_mode,command_skills,request_id,content,error,activity,created_at,started_at,completed_at,updated_at`

func (s *Store) CreateChatRun(ctx context.Context, x ChatRun) (ChatRun, error) {
	if x.CommandSkills == nil {
		x.CommandSkills = []string{}
	}
	return scanChatRun(s.DB.QueryRow(ctx, `INSERT INTO chat_runs(id,session_id,owner_subject,status,research_mode,thinking_mode,command_mode,command_skills,activity) SELECT $1,s.id,s.owner_subject,'queued',$3,$4,$5,$6,$7 FROM chat_sessions s WHERE s.id=$2 AND s.owner_subject=$8 RETURNING `+chatRunCols, x.ID, x.SessionID, x.ResearchMode, x.ThinkingMode, x.CommandMode, x.CommandSkills, x.Activity, x.OwnerSubject))
}
func (s *Store) ChatRun(ctx context.Context, owner, id string) (ChatRun, error) {
	return scanChatRun(s.DB.QueryRow(ctx, `SELECT `+chatRunCols+` FROM chat_runs WHERE id=$1 AND owner_subject=$2`, id, owner))
}
func (s *Store) LatestChatRun(ctx context.Context, owner, sessionID string) (ChatRun, error) {
	return scanChatRun(s.DB.QueryRow(ctx, `SELECT `+chatRunCols+` FROM chat_runs WHERE session_id=$1 AND owner_subject=$2 ORDER BY created_at DESC LIMIT 1`, sessionID, owner))
}
func (s *Store) ActiveChatRun(ctx context.Context, owner, sessionID string) (ChatRun, error) {
	return scanChatRun(s.DB.QueryRow(ctx, `SELECT `+chatRunCols+` FROM chat_runs WHERE session_id=$1 AND owner_subject=$2 AND status IN ('queued','running','paused') ORDER BY created_at DESC LIMIT 1`, sessionID, owner))
}
func (s *Store) SetChatRunStatus(ctx context.Context, owner, id, status string) (ChatRun, error) {
	if status != "queued" && status != "running" && status != "paused" && status != "cancelled" {
		return ChatRun{}, errors.New("invalid run status")
	}
	return scanChatRun(s.DB.QueryRow(ctx, `UPDATE chat_runs SET status=$3,started_at=CASE WHEN $3='running' THEN COALESCE(started_at,now()) ELSE started_at END,completed_at=CASE WHEN $3='cancelled' THEN now() ELSE completed_at END,updated_at=now() WHERE id=$1 AND owner_subject=$2 RETURNING `+chatRunCols, id, owner, status))
}

func (s *Store) ResetChatRun(ctx context.Context, owner, id string) (ChatRun, error) {
	return scanChatRun(s.DB.QueryRow(ctx, `UPDATE chat_runs SET status='queued',request_id='',content='',error='',completed_at=NULL,activity=jsonb_build_object('phase','queued','research',COALESCE(activity->'research','{}'::jsonb)||jsonb_build_object('mode',research_mode,'phase','queued'),'thinking',jsonb_build_object('mode',thinking_mode),'commands',jsonb_build_object('mode',command_mode,'skills',command_skills)),updated_at=now() WHERE id=$1 AND owner_subject=$2 RETURNING `+chatRunCols, id, owner))
}
func (s *Store) ChatRuns(ctx context.Context, owner, sessionID string) ([]ChatRun, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+chatRunCols+` FROM chat_runs WHERE session_id=$1 AND owner_subject=$2 ORDER BY created_at ASC LIMIT 200`, sessionID, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatRun{}
	for rows.Next() {
		x, err := scanChatRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (s *Store) UpdateChatRunActivity(ctx context.Context, id string, patch any) error {
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `UPDATE chat_runs SET activity=COALESCE(activity,'{}'::jsonb) || $2::jsonb,updated_at=now() WHERE id=$1`, id, raw)
	return err
}
func (s *Store) CompleteChatRun(ctx context.Context, owner, id, requestID, content string, activity any) (ChatRun, error) {
	raw, _ := json.Marshal(activity)
	return scanChatRun(s.DB.QueryRow(ctx, `UPDATE chat_runs SET status='completed',request_id=$3,content=$4,error='',activity=COALESCE(activity,'{}'::jsonb)||$5::jsonb,completed_at=now(),updated_at=now() WHERE id=$1 AND owner_subject=$2 RETURNING `+chatRunCols, id, owner, requestID, content, raw))
}
func (s *Store) FailChatRun(ctx context.Context, owner, id, requestID, message string, activity any) (ChatRun, error) {
	raw, _ := json.Marshal(activity)
	return scanChatRun(s.DB.QueryRow(ctx, `UPDATE chat_runs SET status='failed',request_id=$3,error=$4,activity=COALESCE(activity,'{}'::jsonb)||$5::jsonb,completed_at=now(),updated_at=now() WHERE id=$1 AND owner_subject=$2 RETURNING `+chatRunCols, id, owner, requestID, message, raw))
}
func (s *Store) UsageMetadata(ctx context.Context, requestID string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := s.DB.QueryRow(ctx, `SELECT metadata FROM usage_ledger WHERE request_id=$1`, requestID).Scan(&raw)
	return raw, err
}

func (s *Store) RecoverInterruptedChatRuns(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `UPDATE chat_runs SET status='failed',error='Run interrupted by a server restart. Press Play to retry.',completed_at=now(),updated_at=now(),activity=COALESCE(activity,'{}'::jsonb)||jsonb_build_object('phase','failed','recoveredAfterRestart',true) WHERE status IN ('queued','running')`)
	return err
}
