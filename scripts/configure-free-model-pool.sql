-- Daiki free-model pool policy.
-- Safe to re-run after provider model registration/migrations.
-- Only models that passed the 2026-09-12 live smoke/quality checks are enabled.

UPDATE provider_models SET
  free_pool_enabled = TRUE,
  free_pool_routes = ARRAY['fast','balanced','deep','vision'],
  free_pool_weight = 35,
  free_pool_priority = 100,
  quality_score = 92,
  context_strategy = 'adaptive'
WHERE litellm_model_name = 'gemini-gemini-3.5-flash-lite' AND status = 'active';

UPDATE provider_models SET
  free_pool_enabled = TRUE,
  free_pool_routes = ARRAY['balanced','deep'],
  free_pool_weight = 20,
  free_pool_priority = 100,
  quality_score = 100,
  rpd_limit = 20,
  context_strategy = 'adaptive'
WHERE litellm_model_name = 'gemini-gemini-3.7-flash' AND status = 'active';

UPDATE provider_models SET
  free_pool_enabled = TRUE,
  free_pool_routes = ARRAY['fast'],
  free_pool_weight = 30,
  free_pool_priority = 100,
  quality_score = 84,
  rpm_limit = 30,
  rpd_limit = 250,
  tpm_limit = 70000,
  context_strategy = 'adaptive'
WHERE litellm_model_name = 'groq-groq-compound-mini' AND status = 'active';

UPDATE provider_models SET
  free_pool_enabled = TRUE,
  free_pool_routes = ARRAY['balanced','deep'],
  free_pool_weight = 30,
  free_pool_priority = 100,
  quality_score = 84,
  rpm_limit = 30,
  rpd_limit = 250,
  tpm_limit = 70000,
  context_strategy = 'adaptive'
WHERE litellm_model_name = 'groq-groq-compound' AND status = 'active';

UPDATE provider_models SET
  free_pool_enabled = TRUE,
  free_pool_routes = ARRAY['balanced','deep'],
  free_pool_weight = 25,
  free_pool_priority = 100,
  quality_score = 96,
  rpm_limit = 30,
  rpd_limit = 1000,
  tpm_limit = 8000,
  context_strategy = 'adaptive'
WHERE litellm_model_name = 'groq-openai-gpt-oss-120b' AND status = 'active';

UPDATE provider_models SET
  free_pool_enabled = TRUE,
  free_pool_routes = ARRAY['fast','balanced'],
  free_pool_weight = 20,
  free_pool_priority = 100,
  quality_score = 88,
  rpm_limit = 30,
  rpd_limit = 1000,
  tpm_limit = 8000,
  context_strategy = 'adaptive'
WHERE litellm_model_name = 'groq-openai-gpt-oss-20b' AND status = 'active';

UPDATE provider_models SET
  free_pool_enabled = TRUE,
  free_pool_routes = ARRAY['deep','vision'],
  free_pool_weight = 20,
  free_pool_priority = 100,
  quality_score = 84,
  rpm_limit = 30,
  rpd_limit = 1000,
  tpm_limit = 8000,
  context_strategy = 'adaptive'
WHERE litellm_model_name = 'groq-qwen-qwen3.8-27b' AND status = 'active';

-- These currently fail live use (OpenRouter 401, OpenCode free API restriction,
-- local endpoint timeout), so keep them out until an admin re-tests and enables them.
UPDATE provider_models SET free_pool_enabled = FALSE, free_pool_routes = ARRAY[]::TEXT[]
WHERE litellm_model_name IN (
  'daiki-aipass-openrouter-google-gemma-4-26b-a4b-it-free',
  'opencode-deepseek-v4-flash-free',
  'test-nvidia-nemotron-3-nano-4b',
  'test-qwen3.8-4b-distill',
  'lmstudio-qwythos-9b'
);
