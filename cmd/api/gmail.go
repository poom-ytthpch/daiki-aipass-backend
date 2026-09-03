package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/poom-ytthpch/daiki-ai-passport-backend/internal/store"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const gmailSendScope = "https://www.googleapis.com/auth/gmail.send"

func (a *app) googleOAuthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     a.cfg.GoogleClientID,
		ClientSecret: a.cfg.GoogleClientSecret,
		RedirectURL:  a.cfg.GmailOAuthRedirectURI,
		Scopes:       []string{"openid", "email", gmailSendScope},
		Endpoint:     google.Endpoint,
	}
}

func randomURLToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func tokenHash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func (a *app) encryptionAEAD() (cipher.AEAD, error) {
	if a.cfg.TokenEncryptionKey == "" {
		return nil, errors.New("token encryption key not configured")
	}
	key, err := base64.StdEncoding.DecodeString(a.cfg.TokenEncryptionKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("token encryption key must be base64-encoded 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (a *app) encryptScopedSecret(scope, raw string) (string, error) {
	aead, err := a.encryptionAEAD()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nil, nonce, []byte(raw), []byte("daiki:"+scope+":v1"))
	payload := append(nonce, sealed...)
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func (a *app) decryptScopedSecret(scope, encoded string) (string, error) {
	aead, err := a.encryptionAEAD()
	if err != nil {
		return "", err
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(payload) <= aead.NonceSize() {
		return "", errors.New("invalid encrypted token")
	}
	raw, err := aead.Open(nil, payload[:aead.NonceSize()], payload[aead.NonceSize():], []byte("daiki:"+scope+":v1"))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (a *app) encryptSecret(raw string) (string, error) {
	return a.encryptScopedSecret("gmail:refresh-token", raw)
}
func (a *app) decryptSecret(encoded string) (string, error) {
	return a.decryptScopedSecret("gmail:refresh-token", encoded)
}
func (a *app) gmailAuthorize(w http.ResponseWriter, r *http.Request) {
	if a.redis == nil || a.cfg.GoogleClientID == "" || a.cfg.GoogleClientSecret == "" || a.cfg.AdminEmail == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "gmail oauth is not configured"})
		return
	}
	state, err := randomURLToken(32)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "unable to create oauth state"})
		return
	}
	stateKey := "gmail-oauth-state:" + tokenHash(state)
	if err := a.redis.Set(r.Context(), stateKey, current(r).Sub, 10*time.Minute).Err(); err != nil {
		writeJSON(w, 503, map[string]string{"error": "oauth state store unavailable"})
		return
	}
	authURL := a.googleOAuthConfig().AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent select_account"),
		oauth2.SetAuthURLParam("include_granted_scopes", "true"),
		oauth2.SetAuthURLParam("login_hint", a.cfg.AdminEmail),
	)
	writeJSON(w, 200, map[string]string{"url": authURL})
}

func (a *app) gmailCallback(w http.ResponseWriter, r *http.Request) {
	if a.store == nil || a.redis == nil {
		writeJSON(w, 503, map[string]string{"error": "gmail integration store unavailable"})
		return
	}
	var in struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || in.Code == "" || in.State == "" {
		writeJSON(w, 400, map[string]string{"error": "invalid oauth callback"})
		return
	}
	stateKey := "gmail-oauth-state:" + tokenHash(in.State)
	subject, err := a.redis.GetDel(r.Context(), stateKey).Result()
	if err != nil || subject != current(r).Sub {
		writeJSON(w, 400, map[string]string{"error": "invalid or expired oauth state"})
		return
	}
	tok, err := a.googleOAuthConfig().Exchange(r.Context(), in.Code)
	if err != nil {
		slog.Warn("gmail oauth code exchange failed", "error", err)
		writeJSON(w, 400, map[string]string{"error": "google authorization failed"})
		return
	}
	if tok.RefreshToken == "" {
		writeJSON(w, 400, map[string]string{"error": "google did not return offline access; reconnect and grant consent"})
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "https://openidconnect.googleapis.com/v1/userinfo", nil)
	req.Header.Set("authorization", "Bearer "+tok.AccessToken)
	resp, err := a.http.Do(req)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "google user verification failed"})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	var profile struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if resp.StatusCode >= 300 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&profile) != nil || !profile.EmailVerified || !strings.EqualFold(strings.TrimSpace(profile.Email), a.cfg.AdminEmail) {
		writeJSON(w, 403, map[string]string{"error": "connect the configured Daiki sender Google account"})
		return
	}
	encrypted, err := a.encryptSecret(tok.RefreshToken)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "token encryption unavailable"})
		return
	}
	scopeText, _ := tok.Extra("scope").(string)
	scopes := strings.Fields(scopeText)
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", gmailSendScope}
	}
	_, err = a.store.UpsertOAuthIntegration(r.Context(), store.OAuthIntegration{Provider: "gmail", Subject: profile.Sub, Email: strings.ToLower(profile.Email), EncryptedRefreshToken: encrypted, Scopes: scopes})
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "unable to save gmail integration"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "email": profile.Email})
}

func (a *app) gmailIntegrationStatus(w http.ResponseWriter, r *http.Request) {
	if a.store == nil {
		writeJSON(w, 503, map[string]string{"error": "integration store unavailable"})
		return
	}
	x, err := a.store.OAuthIntegration(r.Context(), "gmail")
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, 200, map[string]any{"connected": false, "sender": a.cfg.AdminEmail})
		return
	}
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "integration status unavailable"})
		return
	}
	writeJSON(w, 200, map[string]any{"connected": true, "sender": x.Email, "connectedAt": x.ConnectedAt, "updatedAt": x.UpdatedAt, "scopes": x.Scopes})
}

func (a *app) gmailAccessToken(ctx context.Context) (string, error) {
	if a.store == nil {
		return "", errors.New("integration store unavailable")
	}
	x, err := a.store.OAuthIntegration(ctx, "gmail")
	if err != nil {
		return "", err
	}
	refresh, err := a.decryptSecret(x.EncryptedRefreshToken)
	if err != nil {
		return "", err
	}
	src := a.googleOAuthConfig().TokenSource(ctx, &oauth2.Token{RefreshToken: refresh})
	tok, err := src.Token()
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

func (a *app) sendGmail(ctx context.Context, to, subject, htmlBody string) error {
	accessToken, err := a.gmailAccessToken(ctx)
	if err != nil {
		return err
	}
	safeTo := strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(to), "\r", ""), "\n", "")
	if safeTo == "" || !strings.Contains(safeTo, "@") {
		return errors.New("invalid recipient")
	}
	msg := "From: Daiki AI Passport <" + a.cfg.AdminEmail + ">\r\n" +
		"To: " + safeTo + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=UTF-8\r\n\r\n" + htmlBody
	payload, _ := json.Marshal(map[string]string{"raw": base64.RawURLEncoding.EncodeToString([]byte(msg))})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://gmail.googleapis.com/gmail/v1/users/me/messages/send", strings.NewReader(string(payload)))
	req.Header.Set("authorization", "Bearer "+accessToken)
	req.Header.Set("content-type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("gmail send status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return nil
}

func (a *app) gmailTest(w http.ResponseWriter, r *http.Request) {
	body := `<div style="font-family:Arial,sans-serif;max-width:560px"><h2>Daiki AI Passport</h2><p>Gmail OAuth is connected and email delivery is working.</p><p style="color:#777">This test was requested from the Daiki Admin console.</p></div>`
	if err := a.sendGmail(r.Context(), a.cfg.AdminEmail, "Daiki AI Passport · Email delivery test", body); err != nil {
		slog.Warn("gmail test send failed", "error", err)
		writeJSON(w, 502, map[string]string{"error": "gmail delivery failed"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *app) gmailDisconnect(w http.ResponseWriter, r *http.Request) {
	if a.store == nil {
		writeJSON(w, 503, map[string]string{"error": "integration store unavailable"})
		return
	}
	if x, err := a.store.OAuthIntegration(r.Context(), "gmail"); err == nil {
		if refresh, decErr := a.decryptSecret(x.EncryptedRefreshToken); decErr == nil {
			form := url.Values{"token": {refresh}}
			req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "https://oauth2.googleapis.com/revoke", strings.NewReader(form.Encode()))
			req.Header.Set("content-type", "application/x-www-form-urlencoded")
			if resp, revokeErr := a.http.Do(req); revokeErr == nil {
				_ = resp.Body.Close()
			}
		}
	}
	if err := a.store.DeleteOAuthIntegration(r.Context(), "gmail"); err != nil {
		writeJSON(w, 503, map[string]string{"error": "unable to disconnect gmail"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *app) passwordReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in) != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	defer writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "If an account exists for that email, a password reset link has been sent."})
	if email == "" || !strings.Contains(email, "@") || len(email) > 254 || a.store == nil {
		return
	}
	if a.redis != nil {
		key := "password-reset:" + tokenHash(email)
		count, err := a.redis.Incr(r.Context(), key).Result()
		if err != nil {
			slog.Warn("password reset rate limit unavailable", "error", err)
			return
		}
		if count == 1 {
			_ = a.redis.Expire(r.Context(), key, 15*time.Minute).Err()
		}
		if count > 3 {
			return
		}
	}
	adminToken, err := a.kcAdminToken(r.Context())
	if err != nil {
		return
	}
	usersURL := fmt.Sprintf("%s/admin/realms/%s/users?email=%s&exact=true", strings.TrimRight(a.cfg.KeycloakBase, "/"), a.cfg.KeycloakRealm, url.QueryEscape(email))
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, usersURL, nil)
	req.Header.Set("authorization", "Bearer "+adminToken)
	resp, err := a.http.Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	var users []struct {
		ID string `json:"id"`
	}
	if resp.StatusCode >= 300 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&users) != nil || len(users) != 1 || users[0].ID == "" {
		return
	}
	rawToken, err := randomURLToken(32)
	if err != nil {
		return
	}
	if err := a.store.CreatePasswordResetToken(r.Context(), tokenHash(rawToken), users[0].ID, email, time.Now().UTC().Add(15*time.Minute)); err != nil {
		return
	}
	link := a.cfg.AppBaseURL + "/reset-password?token=" + url.QueryEscape(rawToken)
	body := `<div style="font-family:Arial,sans-serif;max-width:560px;color:#28231f"><h2>Reset your Daiki password</h2><p>We received a request to reset your Daiki AI Passport password.</p><p><a href="` + html.EscapeString(link) + `" style="display:inline-block;background:#ff8066;color:#fff;text-decoration:none;padding:12px 18px;border-radius:999px;font-weight:700">Set a new password</a></p><p>This link expires in 15 minutes and can be used once.</p><p style="color:#777;font-size:12px">If you did not request this, you can ignore this email.</p></div>`
	if err := a.sendGmail(r.Context(), email, "Reset your Daiki AI Passport password", body); err != nil {
		slog.Warn("password reset gmail delivery failed", "error", err)
	}
}

func (a *app) passwordResetConfirm(w http.ResponseWriter, r *http.Request) {
	if a.store == nil {
		writeJSON(w, 503, map[string]string{"error": "reset store unavailable"})
		return
	}
	var in struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || len(in.Token) < 20 || len(in.Password) < 8 || len(in.Password) > 256 {
		writeJSON(w, 400, map[string]string{"error": "invalid reset request"})
		return
	}
	userID, _, err := a.store.ConsumePasswordResetToken(r.Context(), tokenHash(in.Token))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "reset link is invalid or expired"})
		return
	}
	adminToken, err := a.kcAdminToken(r.Context())
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "identity service unavailable; request a new reset link"})
		return
	}
	resetURL := fmt.Sprintf("%s/admin/realms/%s/users/%s/reset-password", strings.TrimRight(a.cfg.KeycloakBase, "/"), a.cfg.KeycloakRealm, url.PathEscape(userID))
	payload, _ := json.Marshal(map[string]any{"type": "password", "value": in.Password, "temporary": false})
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPut, resetURL, strings.NewReader(string(payload)))
	req.Header.Set("authorization", "Bearer "+adminToken)
	req.Header.Set("content-type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "identity service unavailable; request a new reset link"})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		writeJSON(w, 400, map[string]string{"error": "password was rejected; request a new reset link and try another password"})
		return
	}
	logoutURL := fmt.Sprintf("%s/admin/realms/%s/users/%s/logout", strings.TrimRight(a.cfg.KeycloakBase, "/"), a.cfg.KeycloakRealm, url.PathEscape(userID))
	logoutReq, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, logoutURL, nil)
	logoutReq.Header.Set("authorization", "Bearer "+adminToken)
	if logoutResp, logoutErr := a.http.Do(logoutReq); logoutErr == nil {
		_ = logoutResp.Body.Close()
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
