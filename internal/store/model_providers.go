package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type ModelProvider struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	ProviderType    string     `json:"providerType"`
	BaseURL         string     `json:"baseUrl"`
	EncryptedAPIKey string     `json:"-"`
	HasAPIKey       bool       `json:"hasApiKey"`
	Enabled         bool       `json:"enabled"`
	LastTestStatus  string     `json:"lastTestStatus"`
	LastTestMessage string     `json:"lastTestMessage"`
	LastTestAt      *time.Time `json:"lastTestAt,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

type ProviderModel struct {
	ID                   string    `json:"id"`
	ProviderID           string    `json:"providerId"`
	UpstreamModel        string    `json:"upstreamModel"`
	LiteLLMModelName     string    `json:"litellmModelName"`
	LiteLLMModelID       string    `json:"litellmModelId,omitempty"`
	Status               string    `json:"status"`
	LastError            string    `json:"lastError,omitempty"`
	MaxInputTokens       int       `json:"maxInputTokens"`
	MaxOutputTokens      int       `json:"maxOutputTokens"`
	TPMLimit             int       `json:"tpmLimit"`
	ITPMLimit            int       `json:"itpmLimit"`
	OTPMLimit            int       `json:"otpmLimit"`
	RPMLimit             int       `json:"rpmLimit"`
	TimeoutSeconds       int       `json:"timeoutSeconds"`
	StreamTimeoutSeconds int       `json:"streamTimeoutSeconds"`
	MaxRetries           int       `json:"maxRetries"`
	ProviderMaxRetries   int       `json:"providerMaxRetries"`
	RetryBackoffMS       int       `json:"retryBackoffMs"`
	ContextStrategy      string    `json:"contextStrategy"`
	ContextTargetTokens  int       `json:"contextTargetTokens"`
	AgentOverheadTokens  int       `json:"agentOverheadTokens"`
	FallbackModelName    string    `json:"fallbackModelName"`
	CreatedAt            time.Time `json:"createdAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

type ModelAlias struct {
	Alias            string    `json:"alias"`
	LiteLLMModelName string    `json:"litellmModelName"`
	UpdatedBy        string    `json:"updatedBy"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

func scanModelProvider(row pgx.Row) (ModelProvider, error) {
	var x ModelProvider
	err := row.Scan(&x.ID, &x.Name, &x.ProviderType, &x.BaseURL, &x.EncryptedAPIKey, &x.Enabled, &x.LastTestStatus, &x.LastTestMessage, &x.LastTestAt, &x.CreatedAt, &x.UpdatedAt)
	x.HasAPIKey = x.EncryptedAPIKey != ""
	return x, err
}
func (s *Store) ModelProviders(ctx context.Context) ([]ModelProvider, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,name,provider_type,base_url,encrypted_api_key,enabled,last_test_status,last_test_message,last_test_at,created_at,updated_at FROM model_providers ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelProvider{}
	for rows.Next() {
		x, err := scanModelProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (s *Store) ModelProvider(ctx context.Context, id string) (ModelProvider, error) {
	return scanModelProvider(s.DB.QueryRow(ctx, `SELECT id,name,provider_type,base_url,encrypted_api_key,enabled,last_test_status,last_test_message,last_test_at,created_at,updated_at FROM model_providers WHERE id=$1`, id))
}
func (s *Store) UpsertModelProvider(ctx context.Context, x ModelProvider) (ModelProvider, error) {
	return scanModelProvider(s.DB.QueryRow(ctx, `INSERT INTO model_providers(id,name,provider_type,base_url,encrypted_api_key,enabled) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(id) DO UPDATE SET name=EXCLUDED.name,provider_type=EXCLUDED.provider_type,base_url=EXCLUDED.base_url,encrypted_api_key=CASE WHEN EXCLUDED.encrypted_api_key='' THEN model_providers.encrypted_api_key ELSE EXCLUDED.encrypted_api_key END,enabled=EXCLUDED.enabled,updated_at=now() RETURNING id,name,provider_type,base_url,encrypted_api_key,enabled,last_test_status,last_test_message,last_test_at,created_at,updated_at`, x.ID, x.Name, x.ProviderType, x.BaseURL, x.EncryptedAPIKey, x.Enabled))
}
func (s *Store) SetProviderTest(ctx context.Context, id, status, message string) error {
	_, err := s.DB.Exec(ctx, `UPDATE model_providers SET last_test_status=$2,last_test_message=$3,last_test_at=now(),updated_at=now() WHERE id=$1`, id, status, message)
	return err
}
func (s *Store) DeleteModelProvider(ctx context.Context, id string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM model_providers WHERE id=$1`, id)
	return err
}

func scanProviderModel(row pgx.Row) (ProviderModel, error) {
	var x ProviderModel
	err := row.Scan(&x.ID, &x.ProviderID, &x.UpstreamModel, &x.LiteLLMModelName, &x.LiteLLMModelID, &x.Status, &x.LastError, &x.MaxInputTokens, &x.MaxOutputTokens, &x.TPMLimit, &x.ITPMLimit, &x.OTPMLimit, &x.RPMLimit, &x.TimeoutSeconds, &x.StreamTimeoutSeconds, &x.MaxRetries, &x.ProviderMaxRetries, &x.RetryBackoffMS, &x.ContextStrategy, &x.ContextTargetTokens, &x.AgentOverheadTokens, &x.FallbackModelName, &x.CreatedAt, &x.UpdatedAt)
	return x, err
}
func (s *Store) ProviderModels(ctx context.Context, providerID string) ([]ProviderModel, error) {
	q := `SELECT id,provider_id,upstream_model,litellm_model_name,COALESCE(litellm_model_id,''),status,last_error,max_input_tokens,max_output_tokens,tpm_limit,itpm_limit,otpm_limit,rpm_limit,timeout_seconds,stream_timeout_seconds,max_retries,provider_max_retries,retry_backoff_ms,context_strategy,context_target_tokens,agent_overhead_tokens,fallback_model_name,created_at,updated_at FROM provider_models`
	args := []any{}
	if providerID != "" {
		q += ` WHERE provider_id=$1`
		args = append(args, providerID)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.DB.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProviderModel{}
	for rows.Next() {
		x, err := scanProviderModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (s *Store) ProviderModel(ctx context.Context, id string) (ProviderModel, error) {
	return scanProviderModel(s.DB.QueryRow(ctx, `SELECT id,provider_id,upstream_model,litellm_model_name,COALESCE(litellm_model_id,''),status,last_error,max_input_tokens,max_output_tokens,tpm_limit,itpm_limit,otpm_limit,rpm_limit,timeout_seconds,stream_timeout_seconds,max_retries,provider_max_retries,retry_backoff_ms,context_strategy,context_target_tokens,agent_overhead_tokens,fallback_model_name,created_at,updated_at FROM provider_models WHERE id=$1`, id))
}
func (s *Store) UpsertProviderModel(ctx context.Context, x ProviderModel) (ProviderModel, error) {
	return scanProviderModel(s.DB.QueryRow(ctx, `INSERT INTO provider_models(id,provider_id,upstream_model,litellm_model_name,litellm_model_id,status,last_error) VALUES($1,$2,$3,$4,NULLIF($5,''),$6,$7) ON CONFLICT(provider_id,upstream_model) DO UPDATE SET litellm_model_name=EXCLUDED.litellm_model_name,litellm_model_id=COALESCE(EXCLUDED.litellm_model_id,provider_models.litellm_model_id),status=EXCLUDED.status,last_error=EXCLUDED.last_error,updated_at=now() RETURNING id,provider_id,upstream_model,litellm_model_name,COALESCE(litellm_model_id,''),status,last_error,max_input_tokens,max_output_tokens,tpm_limit,itpm_limit,otpm_limit,rpm_limit,timeout_seconds,stream_timeout_seconds,max_retries,provider_max_retries,retry_backoff_ms,context_strategy,context_target_tokens,agent_overhead_tokens,fallback_model_name,created_at,updated_at`, x.ID, x.ProviderID, x.UpstreamModel, x.LiteLLMModelName, x.LiteLLMModelID, x.Status, x.LastError))
}

func (s *Store) ProviderModelByLiteLLMName(ctx context.Context, name string) (ProviderModel, error) {
	return scanProviderModel(s.DB.QueryRow(ctx, `SELECT id,provider_id,upstream_model,litellm_model_name,COALESCE(litellm_model_id,''),status,last_error,max_input_tokens,max_output_tokens,tpm_limit,itpm_limit,otpm_limit,rpm_limit,timeout_seconds,stream_timeout_seconds,max_retries,provider_max_retries,retry_backoff_ms,context_strategy,context_target_tokens,agent_overhead_tokens,fallback_model_name,created_at,updated_at FROM provider_models WHERE litellm_model_name=$1`, name))
}

func (s *Store) UpdateProviderModelRuntime(ctx context.Context, actor string, x ProviderModel) (ProviderModel, error) {
	if x.TimeoutSeconds <= 0 || x.TimeoutSeconds > 1800 {
		return ProviderModel{}, errors.New("timeoutSeconds must be between 1 and 1800")
	}
	if x.StreamTimeoutSeconds <= 0 || x.StreamTimeoutSeconds > 1800 {
		return ProviderModel{}, errors.New("streamTimeoutSeconds must be between 1 and 1800")
	}
	if x.MaxRetries < 0 || x.MaxRetries > 8 {
		return ProviderModel{}, errors.New("maxRetries must be between 0 and 8")
	}
	if x.ProviderMaxRetries < 0 || x.ProviderMaxRetries > 8 {
		return ProviderModel{}, errors.New("providerMaxRetries must be between 0 and 8")
	}
	if x.RetryBackoffMS < 0 || x.RetryBackoffMS > 30000 {
		return ProviderModel{}, errors.New("retryBackoffMs must be between 0 and 30000")
	}
	for _, v := range []int{x.MaxInputTokens, x.MaxOutputTokens, x.TPMLimit, x.ITPMLimit, x.OTPMLimit, x.RPMLimit, x.ContextTargetTokens, x.AgentOverheadTokens} {
		if v < 0 {
			return ProviderModel{}, errors.New("token/rate limits must be >= 0")
		}
	}
	switch x.ContextStrategy {
	case "adaptive", "trim", "fallback", "reject":
	default:
		return ProviderModel{}, errors.New("contextStrategy must be adaptive, trim, fallback, or reject")
	}
	out, err := scanProviderModel(s.DB.QueryRow(ctx, `UPDATE provider_models SET max_input_tokens=$2,max_output_tokens=$3,tpm_limit=$4,itpm_limit=$5,otpm_limit=$6,rpm_limit=$7,timeout_seconds=$8,stream_timeout_seconds=$9,max_retries=$10,provider_max_retries=$11,retry_backoff_ms=$12,context_strategy=$13,context_target_tokens=$14,agent_overhead_tokens=$15,fallback_model_name=$16,updated_at=now() WHERE id=$1 RETURNING id,provider_id,upstream_model,litellm_model_name,COALESCE(litellm_model_id,''),status,last_error,max_input_tokens,max_output_tokens,tpm_limit,itpm_limit,otpm_limit,rpm_limit,timeout_seconds,stream_timeout_seconds,max_retries,provider_max_retries,retry_backoff_ms,context_strategy,context_target_tokens,agent_overhead_tokens,fallback_model_name,created_at,updated_at`, x.ID, x.MaxInputTokens, x.MaxOutputTokens, x.TPMLimit, x.ITPMLimit, x.OTPMLimit, x.RPMLimit, x.TimeoutSeconds, x.StreamTimeoutSeconds, x.MaxRetries, x.ProviderMaxRetries, x.RetryBackoffMS, x.ContextStrategy, x.ContextTargetTokens, x.AgentOverheadTokens, x.FallbackModelName))
	if err != nil {
		return ProviderModel{}, err
	}
	b, _ := json.Marshal(out)
	_, _ = s.DB.Exec(ctx, `INSERT INTO access_audit_log(actor_subject,action,target_type,target_id,new_value) VALUES($1,'provider.model.runtime.update','provider_model',$2,$3)`, actor, out.ID, b)
	return out, nil
}

func (s *Store) SetProviderModelState(ctx context.Context, id, status, litellmID, lastError string) error {
	_, err := s.DB.Exec(ctx, `UPDATE provider_models SET status=$2,litellm_model_id=CASE WHEN $3='' THEN litellm_model_id ELSE $3 END,last_error=$4,updated_at=now() WHERE id=$1`, id, status, litellmID, lastError)
	return err
}
func (s *Store) DeleteProviderModel(ctx context.Context, id string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM provider_models WHERE id=$1`, id)
	return err
}

func (s *Store) ModelAliases(ctx context.Context) ([]ModelAlias, error) {
	rows, err := s.DB.Query(ctx, `SELECT alias,litellm_model_name,updated_by,updated_at FROM model_aliases ORDER BY alias`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelAlias{}
	for rows.Next() {
		var x ModelAlias
		if err := rows.Scan(&x.Alias, &x.LiteLLMModelName, &x.UpdatedBy, &x.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (s *Store) ModelAlias(ctx context.Context, alias string) (ModelAlias, error) {
	var x ModelAlias
	err := s.DB.QueryRow(ctx, `SELECT alias,litellm_model_name,updated_by,updated_at FROM model_aliases WHERE alias=$1`, alias).Scan(&x.Alias, &x.LiteLLMModelName, &x.UpdatedBy, &x.UpdatedAt)
	return x, err
}
func (s *Store) SetModelAlias(ctx context.Context, alias, model, actor string) (ModelAlias, error) {
	var x ModelAlias
	err := s.DB.QueryRow(ctx, `INSERT INTO model_aliases(alias,litellm_model_name,updated_by) VALUES($1,$2,$3) ON CONFLICT(alias) DO UPDATE SET litellm_model_name=EXCLUDED.litellm_model_name,updated_by=EXCLUDED.updated_by,updated_at=now() RETURNING alias,litellm_model_name,updated_by,updated_at`, alias, model, actor).Scan(&x.Alias, &x.LiteLLMModelName, &x.UpdatedBy, &x.UpdatedAt)
	return x, err
}
func (s *Store) DeleteAliasesForModel(ctx context.Context, model string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM model_aliases WHERE litellm_model_name=$1`, model)
	return err
}

var ErrProviderNotFound = errors.New("model provider not found")
