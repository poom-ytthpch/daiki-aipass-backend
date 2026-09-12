#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PROVIDERS_TESTED=0
PROVIDERS_FAILED=0

run_provider() {
  local provider="$1" key_var="$2" endpoint="$3" fast="$4" balanced="$5" deep="$6" native_reasoning="${7:-0}"
  local key="${!key_var:-}"
  if [[ -z "$key" ]]; then
    printf 'PROVIDER_SKIP provider=%s reason=missing_%s\n' "$provider" "$key_var"
    return 0
  fi
  printf 'PROVIDER_START provider=%s fast=%s balanced=%s deep=%s\n' "$provider" "$fast" "$balanced" "$deep"
  PROVIDERS_TESTED=$((PROVIDERS_TESTED + 1))
  if DAIKI_LIVE_ROUTE_QUALITY_BENCHMARK=1 \
    DAIKI_LIVE_REASONING_URL="$endpoint" \
    DAIKI_LIVE_REASONING_API_KEY="$key" \
    DAIKI_LIVE_REASONING_TOKEN_FIELD=max_tokens \
    DAIKI_LIVE_REASONING_NATIVE_REASONING="$native_reasoning" \
    DAIKI_LIVE_ROUTE_FAST_MODEL="$fast" \
    DAIKI_LIVE_ROUTE_BALANCED_MODEL="$balanced" \
    DAIKI_LIVE_ROUTE_DEEP_MODEL="$deep" \
    DAIKI_LIVE_ROUTE_PAUSE_MS="${DAIKI_LIVE_ROUTE_PAUSE_MS:-2200}" \
    go test ./cmd/api -run '^TestLiveRouteAnswerQualityBenchmark$' -count=1 -v; then
    printf 'PROVIDER_DONE provider=%s status=pass\n' "$provider"
  else
    PROVIDERS_FAILED=$((PROVIDERS_FAILED + 1))
    printf 'PROVIDER_DONE provider=%s status=fail\n' "$provider"
  fi
}

run_provider \
  mistral MISTRAL_API_KEY \
  'https://api.mistral.ai/v1/chat/completions' \
  "${DAIKI_MISTRAL_FAST_MODEL:-ministral-8b-latest}" \
  "${DAIKI_MISTRAL_BALANCED_MODEL:-mistral-small-latest}" \
  "${DAIKI_MISTRAL_DEEP_MODEL:-mistral-medium-3-5}" \
  0

run_provider \
  gemini GEMINI_API_KEY \
  'https://generativelanguage.googleapis.com/v1beta/openai/chat/completions' \
  "${DAIKI_GEMINI_FAST_MODEL:-gemini-3.5-flash-lite}" \
  "${DAIKI_GEMINI_BALANCED_MODEL:-gemini-3.7-flash}" \
  "${DAIKI_GEMINI_DEEP_MODEL:-gemini-3.8-flash}" \
  1

run_provider \
  opencode-zen OPENCODE_ZEN_API_KEY \
  'https://opencode.ai/zen/v1/chat/completions' \
  "${DAIKI_ZEN_FAST_MODEL:-nemotron-3.5-lightning-free}" \
  "${DAIKI_ZEN_BALANCED_MODEL:-mimo-v2.5-free}" \
  "${DAIKI_ZEN_DEEP_MODEL:-nemotron-3-ultra-free}" \
  0

printf 'PROVIDER_MATRIX_SUMMARY tested=%d failed=%d\n' "$PROVIDERS_TESTED" "$PROVIDERS_FAILED"
if (( PROVIDERS_TESTED == 0 )); then
  printf 'No provider benchmark ran. Export MISTRAL_API_KEY, GEMINI_API_KEY, and/or OPENCODE_ZEN_API_KEY.\n' >&2
  exit 2
fi
if (( PROVIDERS_FAILED > 0 )); then
  exit 1
fi
