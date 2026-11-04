#!/usr/bin/env bash
# Runs the Playwright suite against the built binary, signing in through Keycloak (compose `oidc` profile)
# with a throwaway PostgreSQL.
set -euo pipefail

readonly ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly PROJECT="gedour-e2e"
# 18080 is registered as a redirect URI in deploy/keycloak/realm.json; another port needs a realm change.
readonly HTTP_PORT="${E2E_PORT:-18080}"
readonly METRICS_PORT="${E2E_METRICS_PORT:-19090}"
readonly DB_PORT="${E2E_DB_PORT:-55433}"
readonly KC_PORT="${E2E_KC_PORT:-18081}"
# Published by the keycloak service for the compose gedour-oidc app; unused here but must be free.
readonly OIDC_APP_PORT="${E2E_OIDC_APP_PORT:-18082}"
readonly BIN="${ROOT}/bin/gedour"
readonly LOG="${ROOT}/bin/e2e-server.log"
# Origin is localhost, not 127.0.0.1: the provider redirects there and the Secure cookie is scoped to it.
readonly BASE_URL="http://localhost:${HTTP_PORT}"

compose() {
  GEDOUR_DB_PORT="${DB_PORT}" GEDOUR_KC_PORT="${KC_PORT}" GEDOUR_OIDC_PORT="${OIDC_APP_PORT}" \
    docker compose -f "${ROOT}/compose.yaml" -p "${PROJECT}" --profile oidc "$@"
}

srv=""
cleanup() {
  if [[ -n "${srv}" ]] && kill -0 "${srv}" 2>/dev/null; then
    kill -TERM "${srv}"
    wait "${srv}" || true
  fi
  # Separate compose project: `down -v` never touches the `make dev` volume.
  compose down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

if [[ ! -x "${BIN}" ]]; then
  echo "missing ${BIN}: run 'make build' first" >&2
  exit 1
fi
for port in "${HTTP_PORT}" "${METRICS_PORT}" "${DB_PORT}" "${KC_PORT}" "${OIDC_APP_PORT}"; do
  if (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
    echo "port ${port} in use: set E2E_PORT / E2E_METRICS_PORT / E2E_DB_PORT / E2E_KC_PORT / E2E_OIDC_APP_PORT" >&2
    exit 1
  fi
done

# Keycloak's healthcheck bounds the wait (60 x 3 s); --wait-timeout caps the whole start.
if ! compose up -d --wait --wait-timeout 300 db keycloak; then
  echo "db or keycloak did not become healthy:" >&2
  compose logs --tail 80 keycloak >&2 || true
  exit 1
fi

# env -u: a GEDOUR_DEV_USER exported in the caller's shell would refuse to start next to OIDC.
env -u GEDOUR_DEV_USER \
  GEDOUR_DATABASE_URL="postgres://gedour:gedour@127.0.0.1:${DB_PORT}/gedour?sslmode=disable" \
  GEDOUR_HTTP_ADDR="127.0.0.1:${HTTP_PORT}" \
  GEDOUR_METRICS_ADDR="127.0.0.1:${METRICS_PORT}" \
  GEDOUR_LOG_FORMAT=json \
  GEDOUR_PUBLIC_URL="${BASE_URL}" \
  OIDC_ISSUER="http://localhost:${KC_PORT}/realms/gedour-test" \
  OIDC_CLIENT_ID=gedour \
  OIDC_CLIENT_SECRET=gedour-test-secret \
  "${BIN}" >"${LOG}" 2>&1 &
srv=$!

healthy=0
for _ in $(seq 1 100); do
  if ! kill -0 "${srv}" 2>/dev/null; then
    echo "server died before becoming healthy:" >&2
    cat "${LOG}" >&2
    exit 1
  fi
  if curl -sf "http://127.0.0.1:${HTTP_PORT}/healthz" >/dev/null; then
    healthy=1
    break
  fi
  sleep 0.3
done
if [[ "${healthy}" -ne 1 ]]; then
  echo "server not healthy after 30 s:" >&2
  cat "${LOG}" >&2
  exit 1
fi

E2E_BASE_URL="${BASE_URL}" pnpm --dir "${ROOT}/web" exec playwright test "$@"
