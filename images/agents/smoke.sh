#!/usr/bin/env bash

set -euo pipefail

runtime=${1:?usage: smoke.sh RUNTIME IMAGE}
image=${2:?usage: smoke.sh RUNTIME IMAGE}

case "${runtime}" in
  crewai)
    container_port=8080
    ;;
  hermes)
    # Exercise the historical collision: Hermes' native API also defaults to
    # 8642, so the outward proxy must select a different loopback port.
    container_port=8642
    ;;
  langgraph)
    container_port=8080
    ;;
  openclaw)
    container_port=18789
    ;;
  *)
    echo "unsupported agent runtime: ${runtime}" >&2
    exit 2
    ;;
esac

agent_repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
server_container_name="airunway-${runtime}-server-smoke-$$"
job_container_name="airunway-${runtime}-job-smoke-$$"
host_port=18080
mock_port=18081
access_token=non-secret-smoke-access-token
model_access_token=non-secret-smoke-model-access-token
mock_pid=""

cleanup() {
  docker rm --force "${server_container_name}" >/dev/null 2>&1 || true
  docker rm --force "${job_container_name}" >/dev/null 2>&1 || true
  if [[ -n "${mock_pid}" ]]; then
    kill "${mock_pid}" >/dev/null 2>&1 || true
    wait "${mock_pid}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

AIRUNWAY_MOCK_API_KEY="${model_access_token}" \
  python3 "${agent_repo_root}/images/agents/testdata/openai_mock.py" "${mock_port}" &
mock_pid=$!
for _attempt in $(seq 1 30); do
  if curl --fail --silent --max-time 2 "http://127.0.0.1:${mock_port}/healthz" >/dev/null; then
    break
  fi
  sleep 1
done
curl --fail --silent --max-time 2 "http://127.0.0.1:${mock_port}/healthz" >/dev/null
mock_unauthenticated_status="$(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 2 \
  "http://127.0.0.1:${mock_port}/v1/models")"
if [[ "${mock_unauthenticated_status}" != "401" ]]; then
  echo "model mock accepted a request without its model credential" >&2
  exit 1
fi
curl --fail --silent --max-time 2 \
  --header "Authorization: Bearer ${model_access_token}" \
  "http://127.0.0.1:${mock_port}/v1/models" >/dev/null

docker run --detach \
  --name "${server_container_name}" \
  --read-only \
  --user 65532:65532 \
  --tmpfs /tmp:rw,noexec,nosuid,size=512m \
  --add-host host.docker.internal:host-gateway \
  --mount "type=bind,src=${agent_repo_root}/images/agents/testdata/agent.json,dst=/etc/airunway/agent.json,readonly" \
  --publish "${host_port}:${container_port}" \
  --env "AIRUNWAY_AGENT_PORT=${container_port}" \
  --env "AIRUNWAY_AGENT_API_KEY=${access_token}" \
  --env OPENAI_MODEL=smoke-model \
  --env "OPENAI_BASE_URL=http://host.docker.internal:${mock_port}/v1" \
  --env "OPENAI_API_KEY=${model_access_token}" \
  "${image}" >/dev/null

for _attempt in $(seq 1 60); do
  if curl --fail --silent --max-time 5 "http://127.0.0.1:${host_port}/healthz" >/dev/null; then
    break
  fi
  if [[ "$(docker inspect --format '{{.State.Running}}' "${server_container_name}" 2>/dev/null || true)" != "true" ]]; then
    echo "agent image exited before becoming healthy: ${runtime} (${image})" >&2
    docker logs "${server_container_name}" >&2 || true
    exit 1
  fi
  sleep 2
done

if ! curl --fail --silent --max-time 5 "http://127.0.0.1:${host_port}/healthz" >/dev/null; then
  echo "agent image did not become healthy in time: ${runtime} (${image})" >&2
  docker logs "${server_container_name}" >&2 || true
  exit 1
fi

unauthenticated_status="$(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 5 \
  "http://127.0.0.1:${host_port}/v1/models")"
if [[ "${unauthenticated_status}" != "401" ]]; then
  echo "agent image accepted unauthenticated model discovery: ${runtime} (${image})" >&2
  exit 1
fi
wrong_token_status="$(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 5 \
  --header 'Authorization: Bearer wrong-smoke-access-token' \
  "http://127.0.0.1:${host_port}/v1/models")"
if [[ "${wrong_token_status}" != "401" ]]; then
  echo "agent image accepted an incorrect bearer token: ${runtime} (${image})" >&2
  exit 1
fi
curl --fail --silent --max-time 5 \
  --header "Authorization: Bearer ${access_token}" \
  "http://127.0.0.1:${host_port}/v1/models" >/dev/null

curl --fail-with-body --silent --show-error --max-time 120 \
  --header "Authorization: Bearer ${access_token}" \
  --header 'Content-Type: application/json' \
  --data '{"messages":[{"role":"user","content":"Reply to this smoke test."}]}' \
  "http://127.0.0.1:${host_port}/v1/chat/completions" \
  | python3 -c 'import json,sys; body=json.load(sys.stdin); assert body["choices"][0]["message"]["content"]'

if [[ "${runtime}" == "langgraph" ]]; then
  curl --fail-with-body --silent --show-error --max-time 120 \
    --header "Authorization: Bearer ${access_token}" \
    --header 'Content-Type: application/json' \
    --data '{"messages":[{"role":"user","content":"airunway-isolation-first"}]}' \
    "http://127.0.0.1:${host_port}/v1/chat/completions" >/dev/null
  curl --fail-with-body --silent --show-error --max-time 120 \
    --header "Authorization: Bearer ${access_token}" \
    --header 'Content-Type: application/json' \
    --data '{"messages":[{"role":"user","content":"airunway-isolation-second"}]}' \
    "http://127.0.0.1:${host_port}/v1/chat/completions" \
    | python3 -c 'import json,sys; body=json.load(sys.stdin); assert body["choices"][0]["message"]["content"] == "state isolated"'
fi

docker run --detach \
  --name "${job_container_name}" \
  --read-only \
  --user 65532:65532 \
  --tmpfs /tmp:rw,noexec,nosuid,size=512m \
  --add-host host.docker.internal:host-gateway \
  --mount "type=bind,src=${agent_repo_root}/images/agents/testdata/agent-job.json,dst=/etc/airunway/agent.json,readonly" \
  --env AIRUNWAY_AGENT_MODE=job \
  --env OPENAI_MODEL=smoke-model \
  --env "OPENAI_BASE_URL=http://host.docker.internal:${mock_port}/v1" \
  --env "OPENAI_API_KEY=${model_access_token}" \
  "${image}" >/dev/null

for _attempt in $(seq 1 90); do
  if [[ "$(docker inspect --format '{{.State.Running}}' "${job_container_name}" 2>/dev/null || true)" != "true" ]]; then
    break
  fi
  sleep 2
done

if [[ "$(docker inspect --format '{{.State.Running}}' "${job_container_name}" 2>/dev/null || true)" == "true" ]]; then
  echo "agent job did not finish in time: ${runtime} (${image})" >&2
  docker logs "${job_container_name}" >&2 || true
  exit 1
fi
job_exit_code="$(docker inspect --format '{{.State.ExitCode}}' "${job_container_name}")"
if [[ "${job_exit_code}" != "0" ]]; then
  echo "agent job exited with status ${job_exit_code}: ${runtime} (${image})" >&2
  docker logs "${job_container_name}" >&2 || true
  exit 1
fi
if ! docker logs "${job_container_name}" 2>&1 | grep -qi 'smoke response'; then
  echo "agent job did not print the expected result: ${runtime} (${image})" >&2
  docker logs "${job_container_name}" >&2 || true
  exit 1
fi

echo "agent image server + job smoke tests passed: ${runtime} (${image})"
