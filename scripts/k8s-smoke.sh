#!/usr/bin/env bash
# 001-AC28: deploys deploy/k8s (through the test overlay deploy/k8s/test) on a throwaway kind cluster with
# PostgreSQL started only once gedour waits for it, then checks 2 web + 2 worker pods ready without a restart,
# /healthz through the Services, /mcp with an API key, a sandboxed script dry run on web, and a watch run plus a
# script delivery on a worker. Then `docker compose up` on a clean project serves the dev UI.
# Needs the image from `make docker-build` (gedour:local). Every cluster, container and image it creates is
# removed.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT
readonly KIND="${KIND:-kind}"
# First kind release whose default node runs Kubernetes 1.30, where the preStop `sleep` action is on.
readonly MIN_KIND="0.23.0"
readonly CLUSTER="${K8S_SMOKE_CLUSTER:-gedour-smoke}"
readonly NS="gedour-smoke"
readonly IMAGE="gedour:local"
readonly FAKE_IMAGE="gedour-fakeupstream:smoke"
# Read from the manifest so the two never drift.
PG_IMAGE="$(sed -n 's/^ *image: *\(postgres:[^ ]*\)$/\1/p' "${ROOT}/deploy/k8s/test/postgres.yaml")"
readonly PG_IMAGE
readonly WEB_PORT="${K8S_SMOKE_PORT:-18480}"
readonly FAKE_PORT="${K8S_SMOKE_FAKE_PORT:-18481}"
readonly COMPOSE_PROJECT="gedour-compose-smoke"
readonly COMPOSE_HTTP_PORT="${COMPOSE_SMOKE_PORT:-18482}"
readonly COMPOSE_METRICS_PORT="${COMPOSE_SMOKE_METRICS_PORT:-18483}"
readonly COMPOSE_DB_PORT="${COMPOSE_SMOKE_DB_PORT:-55434}"
readonly OUT="${K8S_SMOKE_PODS_OUT:-}"
readonly MCP_VERSION="2026-07-28"
readonly MCP_BODY='{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"'"${MCP_VERSION}"'","io.modelcontextprotocol/clientInfo":{"name":"smoke","version":"0"},"io.modelcontextprotocol/clientCapabilities":{}}}}'

tmpdir="$(mktemp -d)"
forwards=()
cluster_created=0
compose_started=0
fake_built=0

compose() {
  GEDOUR_HTTP_PORT="${COMPOSE_HTTP_PORT}" GEDOUR_METRICS_PORT="${COMPOSE_METRICS_PORT}" \
    GEDOUR_DB_PORT="${COMPOSE_DB_PORT}" GEDOUR_MAILPIT_SMTP_PORT=0 GEDOUR_MAILPIT_PORT=0 \
    docker compose -f "${ROOT}/compose.yaml" -p "${COMPOSE_PROJECT}" "$@"
}

cleanup() {
  local pid
  for pid in "${forwards[@]}"; do
    kill "${pid}" 2>/dev/null || true
  done
  if [[ "${cluster_created}" -eq 1 ]]; then
    "${KIND}" delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || echo "could not delete kind cluster ${CLUSTER}" >&2
  fi
  if [[ "${compose_started}" -eq 1 ]]; then
    # --rmi local: the image compose built for this project goes too.
    compose down -v --rmi local >/dev/null 2>&1 || echo "could not remove compose project ${COMPOSE_PROJECT}" >&2
  fi
  if [[ "${fake_built}" -eq 1 ]]; then
    docker image rm "${FAKE_IMAGE}" >/dev/null 2>&1 || echo "could not remove image ${FAKE_IMAGE}" >&2
  fi
  rm -rf "${tmpdir}"
}
trap cleanup EXIT

fail() {
  echo "k8s-smoke: $*" >&2
  exit 1
}

# Pod state for a CI failure: who is unready, restarted or killed, and the probe events saying why.
# dump_pods: pod and container state, events and the logs of restarted containers, for a failure report.
dump_pods() {
  local pod
  kubectl get pods -o wide --request-timeout=20s >&2 || true
  kubectl get pods -o json --request-timeout=20s | jq -r '.items[] | .metadata.name as $p | .status.containerStatuses[]?
    | "\($p) ready=\(.ready) restarts=\(.restartCount) state=\(.state // {} | keys[0]) last=\(.lastState | tojson)"' >&2 || true
  # events.k8s.io events have no lastTimestamp: sort on creation so the newest 40 are kept.
  kubectl get events --sort-by=.metadata.creationTimestamp --request-timeout=20s | tail -n 40 >&2 || true
  # A container that exited left its last words in the previous instance's log.
  for pod in $(kubectl get pods -o json --request-timeout=20s | jq -r '.items[] | select(any(.status.containerStatuses[]?; .restartCount > 0)) | .metadata.name' 2>/dev/null); do
    echo "== previous logs of ${pod}" >&2
    kubectl logs "${pod}" --previous --tail 30 --request-timeout=20s >&2 || true
  done
}

for cmd in kubectl docker jq curl go; do
  command -v "${cmd}" >/dev/null || fail "missing ${cmd}"
done
command -v "${KIND}" >/dev/null || fail "kind not found: install kind >= v${MIN_KIND} or set KIND=<path>"
kind_version="$("${KIND}" version | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | head -1)" || kind_version=""
[[ -n "${kind_version}" ]] || fail "could not read the version of ${KIND}"
if [[ "$(printf '%s\n' "${MIN_KIND}" "${kind_version#v}" | sort -V | head -1)" != "${MIN_KIND}" ]]; then
  fail "${KIND} is ${kind_version}: its node runs Kubernetes < 1.30 (no preStop sleep action); need kind >= v${MIN_KIND} (KIND=<path>)"
fi
docker image inspect "${IMAGE}" >/dev/null 2>&1 || fail "missing image ${IMAGE}: run 'make docker-build'"
if "${KIND}" get clusters 2>/dev/null | grep -x "${CLUSTER}" >/dev/null; then
  fail "kind cluster ${CLUSTER} already exists: delete it or set K8S_SMOKE_CLUSTER"
fi
for port in "${WEB_PORT}" "${FAKE_PORT}" "${COMPOSE_HTTP_PORT}" "${COMPOSE_METRICS_PORT}" "${COMPOSE_DB_PORT}"; do
  if (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
    fail "port ${port} in use: set K8S_SMOKE_PORT / K8S_SMOKE_FAKE_PORT / COMPOSE_SMOKE_PORT / COMPOSE_SMOKE_METRICS_PORT / COMPOSE_SMOKE_DB_PORT"
  fi
done

echo "== fake upstream image"
CGO_ENABLED=0 go -C "${ROOT}" build -o "${tmpdir}/fakeupstream" ./cmd/fakeupstream || fail "could not build cmd/fakeupstream"
fake_built=1
docker build -q -t "${FAKE_IMAGE}" -f - "${tmpdir}" >/dev/null <<'EOF' || fail "could not build ${FAKE_IMAGE}"
FROM gcr.io/distroless/static-debian13:nonroot
COPY fakeupstream /fakeupstream
ENTRYPOINT ["/fakeupstream"]
EOF

[[ -n "${PG_IMAGE}" ]] || fail "no postgres image in deploy/k8s/test/postgres.yaml"

echo "== kind cluster ${CLUSTER}"
export KUBECONFIG="${tmpdir}/kubeconfig"
cluster_created=1
"${KIND}" create cluster --name "${CLUSTER}" --kubeconfig "${KUBECONFIG}" --wait 180s || fail "kind create cluster failed"
"${KIND}" load docker-image --name "${CLUSTER}" "${IMAGE}" "${FAKE_IMAGE}" || fail "kind load failed"
# Pulled by the node up front: left to the scale-up, the registry pull ran past the rollout timeout. Not kind-loaded:
# a runner's partial multi-platform index fails `ctr import --all-platforms`.
docker exec "${CLUSTER}-control-plane" crictl pull "${PG_IMAGE}" >/dev/null || fail "node could not pull ${PG_IMAGE}"

echo "== deploy"
kubectl kustomize "${ROOT}/deploy/k8s" >/dev/null || fail "deploy/k8s does not render"
kubectl create namespace "${NS}" >/dev/null
kubectl config set-context --current --namespace "${NS}" >/dev/null
# The plain base must pass the API server's validation too, not only the test overlay built on it.
kubectl apply -k "${ROOT}/deploy/k8s" --dry-run=server >/dev/null || fail "deploy/k8s rejected by the API server"
# PostgreSQL starts at 0 replicas: gedour must wait and retry, then become ready without a restart
# (checked with the restart count below).
kubectl apply -k "${ROOT}/deploy/k8s/test" >/dev/null || fail "kubectl apply -k deploy/k8s/test failed"
start=${SECONDS}
db_wait() { kubectl logs deployment/gedour-web 2>/dev/null | grep '"msg":"db_unreachable"' >/dev/null; }
for _ in $(seq 1 120); do
  db_wait && break
  sleep 1
done
db_wait || fail "gedour-web never logged db_unreachable with postgres stopped"
echo "gedour waits for postgres: db_unreachable logged after $((SECONDS - start)) s"
kubectl scale deployment/postgres --replicas 1 >/dev/null || fail "could not start postgres"
if ! kubectl rollout status deployment/postgres --timeout 180s >/dev/null; then
  dump_pods
  fail "postgres not ready"
fi
for d in fakeupstream gedour-web gedour-worker; do
  if ! kubectl rollout status "deployment/${d}" --timeout 300s >/dev/null; then
    dump_pods
    kubectl logs "deployment/${d}" --tail 50 >&2 || true
    fail "deployment ${d} not ready"
  fi
done
echo "ready after $((SECONDS - start)) s"
for d in gedour-web gedour-worker; do
  ready="$(kubectl get "deployment/${d}" -o jsonpath='{.status.readyReplicas}')"
  if [[ "${ready}" != "2" ]]; then
    dump_pods
    kubectl logs -l "app.kubernetes.io/component=${d#gedour-}" --prefix --tail 30 >&2 || true
    fail "${d}: ${ready:-0} ready replicas, want 2"
  fi
done
kubectl get pods -o wide
if [[ -n "${OUT}" ]]; then
  kubectl get pods -o wide >"${OUT}"
fi

echo "== /healthz and /livez through the Services"
# The API server's service proxy goes through the Service's endpoints, not one chosen pod.
health="$(kubectl get --raw "/api/v1/namespaces/${NS}/services/gedour:http/proxy/healthz")" || fail "web /healthz failed"
echo "web: ${health}"
health="$(kubectl get --raw "/api/v1/namespaces/${NS}/services/gedour-worker:metrics/proxy/healthz")" || fail "worker /healthz failed"
echo "worker: ${health}"
for svc in gedour gedour-worker; do
  kubectl get --raw "/api/v1/namespaces/${NS}/services/${svc}:metrics/proxy/livez" >/dev/null || fail "${svc} /livez failed"
done

# Dev mode answers /api on a loopback Host only: port-forward makes it localhost.
port_forward() {
  local target="$1" local_port="$2" remote_port="$3"
  kubectl port-forward "${target}" "${local_port}:${remote_port}" >"${tmpdir}/pf-${local_port}.log" 2>&1 &
  forwards+=("$!")
  for _ in $(seq 1 50); do
    # Any HTTP answer means the forward is up.
    if curl -s -o /dev/null "http://localhost:${local_port}/"; then
      return 0
    fi
    sleep 0.2
  done
  cat "${tmpdir}/pf-${local_port}.log" >&2
  fail "port-forward to ${target} did not come up"
}
port_forward svc/gedour "${WEB_PORT}" 8080
port_forward svc/fakeupstream "${FAKE_PORT}" 8091
readonly BASE="http://localhost:${WEB_PORT}"
readonly JAR="${tmpdir}/cookies"

csrf="$(curl -sf -c "${JAR}" -b "${JAR}" "${BASE}/api/v1/me" | jq -r .csrf_token)" || fail "GET /api/v1/me failed"
[[ -n "${csrf}" && "${csrf}" != "null" ]] || fail "no CSRF token from /api/v1/me"
# api METHOD PATH [JSON]: prints the body, fails on a non-2xx answer.
api() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -f -b "${JAR}" -c "${JAR}" -X "${method}" -H "X-CSRF-Token: ${csrf}")
  if [[ -n "${body}" ]]; then
    args+=(-H "Content-Type: application/json" --data "${body}")
  fi
  curl "${args[@]}" "${BASE}${path}"
}
# wait_for SECONDS COMMAND...: retries COMMAND every second.
wait_for() {
  local limit="$1"
  shift
  for _ in $(seq 1 "${limit}"); do
    if "$@"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

echo "== /mcp with an API key, behind the deployed manifests"
key="$(api POST /api/v1/api-keys '{"name":"smoke","scope":"read"}' | jq -r .key)" || fail "could not create an API key"
[[ -n "${key}" && "${key}" != "null" ]] || fail "no key from POST /api/v1/api-keys"
# mcp_post [CURL_ARG...]: prints the body, then the status code on the last line.
mcp_post() {
  curl -sS --max-time 30 -w '\n%{http_code}' -X POST -H "Accept: application/json, text/event-stream" -H "Content-Type: application/json" \
    -H "MCP-Protocol-Version: ${MCP_VERSION}" -H "Mcp-Method: tools/list" "$@" --data "${MCP_BODY}" "${BASE}/mcp"
}
unauth="$(mcp_post)" || fail "POST /mcp failed"
[[ "${unauth##*$'\n'}" == "401" ]] || fail "/mcp without a key did not answer 401: ${unauth}"
mcp="$(mcp_post -H "Authorization: Bearer ${key}")" || fail "POST /mcp failed"
body="${mcp%$'\n'*}"
code="${mcp##*$'\n'}"
[[ "${code}" == "200" ]] || fail "/mcp with a key did not answer 200: ${mcp}"
listed="$(jq '.result.tools | length' <<<"${body}")" || fail "/mcp answered no JSON: ${mcp}"
[[ "${listed}" -gt 0 ]] || fail "/mcp listed no tool: ${mcp}"
echo "tools listed: ${listed}"

echo "== script dry run on web (sandbox child under the pod's securityContext)"
dry="$(api POST /api/v1/scripts/dry-run \
  '{"source":"function main(event) { log(\"type\", event.type); http.fetch(\"https://example.com/x\"); return {seen: event.type}; }","sample":{"type":"release.published"}}')" \
  || fail "dry run request failed"
echo "${dry}" | jq -c '{ok, outcome, error, logs, result, fetches: (.fetches | length), duration_ms}'
[[ "$(jq -r .outcome <<<"${dry}")" == "ok" ]] || fail "dry run outcome is not ok"
# A child past its 192 MiB cap dies alone; the pod limit has room for it (the restart check below).
hog="$(api POST /api/v1/scripts/dry-run \
  '{"source":"function main() { const a = []; for (;;) a.push({n: a.length, s: \"x\".repeat(64)}); }","sample":{"type":"release.published"}}')" \
  || fail "memory hog dry run request failed"
echo "${hog}" | jq -c '{outcome, error, duration_ms}'
[[ "$(jq -r .outcome <<<"${hog}")" == "killed" ]] || fail "memory hog was not killed by the sandbox"

echo "== watch run and script delivery on a worker"
readonly UPSTREAM="http://fakeupstream:8091"
watch_id="$(api POST /api/v1/watches \
  "{\"name\":\"smoke releases\",\"kind\":\"github_repo\",\"config\":{\"repo\":\"golang/go\",\"api_url\":\"${UPSTREAM}\"}}" | jq -r .id)" \
  || fail "create watch failed"
baseline() { [[ "$(api GET "/api/v1/watches/${watch_id}" | jq -r .last_status)" == "ok" ]]; }
wait_for 60 baseline || fail "watch baseline run never succeeded: $(api GET "/api/v1/watches/${watch_id}")"
script_id="$(api POST /api/v1/scripts \
  "{\"name\":\"smoke forward\",\"source\":\"function main(event) { http.fetch(\\\"${UPSTREAM}/_hooks/smoke\\\", {method: \\\"POST\\\", body: event.summary}); }\"}" \
  | jq -r .id)" || fail "create script failed"
channel_id="$(api POST /api/v1/channels \
  "{\"name\":\"smoke script\",\"type\":\"script\",\"config\":{\"script_id\":\"${script_id}\"}}" | jq -r .id)" \
  || fail "create channel failed"
api POST /api/v1/rules \
  "{\"name\":\"smoke rule\",\"watch_id\":\"${watch_id}\",\"filter\":\"event.type == \\\"release.published\\\"\",\"channel_ids\":[\"${channel_id}\"]}" \
  >/dev/null || fail "create rule failed"
curl -sf -X POST "http://localhost:${FAKE_PORT}/_fixture/b" || fail "could not switch the fake upstream to fixture b"
api POST "/api/v1/watches/${watch_id}/run" >/dev/null || fail "run watch failed"
events() { [[ "$(api GET "/api/v1/watches/${watch_id}/events" | jq '.items | length')" -gt 0 ]]; }
wait_for 60 events || fail "no events after the upstream changed"
echo "events: $(api GET "/api/v1/watches/${watch_id}/events" | jq -c '[.items[].type]')"
sent() {
  [[ "$(api GET "/api/v1/deliveries?channel_id=${channel_id}&status=sent" | jq '.items | length')" -ge 2 ]]
}
if ! wait_for 60 sent; then
  api GET "/api/v1/deliveries?channel_id=${channel_id}" >&2
  fail "script deliveries not sent"
fi
echo "script deliveries sent: $(curl -sf "http://localhost:${FAKE_PORT}/_received" | jq -c '[.[] | select(.endpoint == "smoke") | .body]')"

echo "== restarts and memory"
restarts="$(kubectl get pods -l app.kubernetes.io/name=gedour -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}' | sort -u)"
[[ "${restarts}" == "0" ]] || fail "gedour pods restarted: ${restarts}"
# memory.peak of the pod cgroup counts the sandbox children too: the figure the memory limits size for.
# Informational: a node without cgroup v2 only loses the figure.
while read -r pod uid; do
  peak="$(docker exec "${CLUSTER}-control-plane" sh -c \
    "cat \$(find /sys/fs/cgroup -maxdepth 5 -type d -name '*pod${uid//-/_}.slice')/memory.peak" 2>/dev/null)" || peak=""
  if [[ "${peak}" =~ ^[0-9]+$ ]]; then
    echo "${pod}: memory peak $((peak / 1048576)) MiB"
  else
    echo "${pod}: memory peak unknown"
  fi
done < <(kubectl get pods -l app.kubernetes.io/name=gedour -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.uid}{"\n"}{end}')

echo "== compose: clean project, dev mode UI"
compose down -v >/dev/null 2>&1 || true
compose_started=1
if ! compose up -d --build --wait --wait-timeout 300 db mailpit gedour >/dev/null; then
  compose logs --tail 50 gedour >&2 || true
  fail "compose up failed"
fi
readonly CBASE="http://127.0.0.1:${COMPOSE_HTTP_PORT}"
ui_up() { curl -sf "${CBASE}/healthz" >/dev/null; }
wait_for 60 ui_up || fail "compose gedour never answered /healthz"
curl -sf "${CBASE}/" | grep '<div id="root">' >/dev/null || fail "compose gedour does not serve the UI"
who="$(curl -sf "${CBASE}/api/v1/me" | jq -r .email)" || fail "compose GET /api/v1/me failed"
[[ "${who}" == "dev@local" ]] || fail "compose dev mode user is ${who}, want dev@local"
curl -sf "http://127.0.0.1:${COMPOSE_METRICS_PORT}/metrics" | grep '^gedour_build_info' >/dev/null || fail "compose /metrics has no gedour_build_info"
echo "compose: UI served, signed in as ${who}"

echo "k8s-smoke: OK"
