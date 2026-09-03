package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type OAuthIntegration struct {
	Provider              string    `json:"provider"`
	Subject               string    `json:"subject"`
	Email                 string    `json:"email"`
	EncryptedRefreshToken string    `json:"-"`
	Scopes                []string  `json:"scopes"`
	ConnectedAt           time.Time `json:"connectedAt"`
	UpdatedAt             time.Time `json:"updatedAt"`
}

func (s *Store) UpsertOAuthIntegration(ctx context.Context, x OAuthIntegration) (OAuthIntegration, error) {
	row := s.DB.QueryRow(ctx, `INSERT INTO oauth_integrations(provider,subject,email,encrypted_refresh_token,scopes) VALUES($1,$2,$3,$4,$5)
        ON CONFLICT(provider) DO UPDATE SET subject=EXCLUDED.subject,email=EXCLUDED.email,encrypted_refresh_token=EXCLUDED.encrypted_refresh_token,scopes=EXCLUDED.scopes,updated_at=now()
        RETURNING provider,subject,email,encrypted_refresh_token,scopes,connected_at,updated_at`, x.Provider, x.Subject, x.Email, x.EncryptedRefreshToken, x.Scopes)
	err := row.Scan(&x.Provider, &x.Subject, &x.Email, &x.EncryptedRefreshToken, &x.Scopes, &x.ConnectedAt, &x.UpdatedAt)
	return x, err
}

func (s *Store) OAuthIntegration(ctx context.Context, provider string) (OAuthIntegration, error) {
	var x OAuthIntegration
	err := s.DB.QueryRow(ctx, `SELECT provider,subject,email,encrypted_refresh_token,scopes,connected_at,updated_at FROM oauth_integrations WHERE provider=$1`, provider).
		Scan(&x.Provider, &x.Subject, &x.Email, &x.EncryptedRefreshToken, &x.Scopes, &x.ConnectedAt, &x.UpdatedAt)
	return x, err
}

func (s *Store) DeleteOAuthIntegration(ctx context.Context, provider string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM oauth_integrations WHERE provider=$1`, provider)
	return err
}

func (s *Store) CreatePasswordResetToken(ctx context.Context, hash, userID, email string, expiresAt time.Time) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO password_reset_tokens(token_hash,keycloak_user_id,email,expires_at) VALUES($1,$2,$3,$4)`, hash, userID, email, expiresAt)
	return err
}

func (s *Store) ConsumePasswordResetToken(ctx context.Context, hash string) (userID, email string, err error) {
	err = s.DB.QueryRow(ctx, `UPDATE password_reset_tokens SET used_at=now() WHERE token_hash=$1 AND used_at IS NULL AND expires_at>now() RETURNING keycloak_user_id,email`, hash).Scan(&userID, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", errors.New("invalid or expired reset token")
	}
	return userID, email, err
}
