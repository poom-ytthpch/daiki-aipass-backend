package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type AuditEntry struct {
	ID           int64           `json:"id"`
	ActorSubject string          `json:"actorSubject"`
	Action       string          `json:"action"`
	TargetType   string          `json:"targetType"`
	TargetID     string          `json:"targetId"`
	OldValue     json.RawMessage `json:"oldValue,omitempty"`
	NewValue     json.RawMessage `json:"newValue,omitempty"`
	CreatedAt    time.Time       `json:"createdAt"`
}

func (s *Store) AuditLog(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.DB.Query(ctx, `SELECT id,actor_subject,action,target_type,target_id,old_value,new_value,created_at FROM access_audit_log ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AuditEntry, 0, limit)
	for rows.Next() {
		var x AuditEntry
		if err := rows.Scan(&x.ID, &x.ActorSubject, &x.Action, &x.TargetType, &x.TargetID, &x.OldValue, &x.NewValue, &x.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) UpsertManagedUser(ctx context.Context, actor, subject, email, name string, roles []string, status string) (User, error) {
	if subject == "" || email == "" {
		return User{}, errors.New("subject and email are required")
	}
	if status == "" {
		status = "pending"
	}
	if status != "pending" && status != "approved" && status != "suspended" && status != "rejected" {
		return User{}, errors.New("invalid user status")
	}
	approved := status == "approved"
	row := s.DB.QueryRow(ctx, `INSERT INTO app_users(subject,email,display_name,auth_provider,status,roles,approved_by,approved_at)
		VALUES($1,$2,$3,'email',$4,$5,CASE WHEN $6 THEN $7 ELSE NULL END,CASE WHEN $6 THEN now() ELSE NULL END)
		ON CONFLICT(subject) DO UPDATE SET email=EXCLUDED.email,display_name=EXCLUDED.display_name,status=EXCLUDED.status,roles=EXCLUDED.roles,updated_at=now(),approved_by=CASE WHEN $6 THEN $7 ELSE app_users.approved_by END,approved_at=CASE WHEN $6 THEN COALESCE(app_users.approved_at,now()) ELSE app_users.approved_at END
		RETURNING subject,email,display_name,auth_provider,status,roles,created_at,updated_at,last_login_at,approved_by,approved_at`, subject, email, name, status, roles, approved, actor)
	u, err := scanUser(row)
	if err != nil {
		return User{}, err
	}
	newJSON, _ := json.Marshal(map[string]any{"email": u.Email, "status": u.Status, "roles": u.Roles})
	_, _ = s.DB.Exec(ctx, `INSERT INTO access_audit_log(actor_subject,action,target_type,target_id,new_value) VALUES($1,'user.create','user',$2,$3)`, actor, subject, newJSON)
	return u, nil
}

func (s *Store) SetUserRoles(ctx context.Context, actor, subject string, roles []string) (User, error) {
	old, err := s.User(ctx, subject)
	if err != nil {
		return User{}, err
	}
	u, err := scanUser(s.DB.QueryRow(ctx, `UPDATE app_users SET roles=$2,updated_at=now() WHERE subject=$1 RETURNING subject,email,display_name,auth_provider,status,roles,created_at,updated_at,last_login_at,approved_by,approved_at`, subject, roles))
	if err != nil {
		return User{}, err
	}
	oldJSON, _ := json.Marshal(map[string]any{"roles": old.Roles})
	newJSON, _ := json.Marshal(map[string]any{"roles": u.Roles})
	_, _ = s.DB.Exec(ctx, `INSERT INTO access_audit_log(actor_subject,action,target_type,target_id,old_value,new_value) VALUES($1,'user.roles.update','user',$2,$3,$4)`, actor, subject, oldJSON, newJSON)
	return u, nil
}
