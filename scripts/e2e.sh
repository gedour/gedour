#!/usr/bin/env bash
# Runs the Playwright suite against the built binary and a throwaway PostgreSQL from compose.
set -euo pipefail

readonly ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly PROJECT="gedour-e2e"
readonly HTTP_PORT="${E2E_PORT:-18080}"
readonly METRICS_PORT="${E2E_METRICS_PORT:-19090}"
readonly DB_PORT="${E2E_DB_PORT:-55433}"
readonly BIN="${ROOT}/bin/gedour"
readonly LOG="${ROOT}/bin/e2e-server.log"

srv=""
cleanup() {
  if [[ -n "${srv}" ]] && kill -0 "${srv}" 2>/dev/null; then
    kill -TERM "${srv}"
    wait "${srv}" || true
  fi
  # Separate compose project: `down -v` never touches the `make dev` volume.
  GEDOUR_DB_PORT="${DB_PORT}" docker compose -f "${ROOT}/compose.yaml" -p "${PROJECT}" down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

if [[ ! -x "${BIN}" ]]; then
  echo "missing ${BIN}: run 'make build' first" >&2
  exit 1
fi
for port in "${HTTP_PORT}" "${METRICS_PORT}" "${DB_PORT}"; do
  if (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
    echo "port ${port} in use: set E2E_PORT / E2E_METRICS_PORT / E2E_DB_PORT" >&2
    exit 1
  fi
done

GEDOUR_DB_PORT="${DB_PORT}" docker compose -f "${ROOT}/compose.yaml" -p "${PROJECT}" up -d --wait db

GEDOUR_DATABASE_URL="postgres://gedour:gedour@127.0.0.1:${DB_PORT}/gedour?sslmode=disable" \
  GEDOUR_HTTP_ADDR="127.0.0.1:${HTTP_PORT}" \
  GEDOUR_METRICS_ADDR="127.0.0.1:${METRICS_PORT}" \
  GEDOUR_LOG_FORMAT=json \
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

E2E_BASE_URL="http://127.0.0.1:${HTTP_PORT}" pnpm --dir "${ROOT}/web" exec playwright test "$@"
