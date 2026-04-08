#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG_PATH="${ROOT_DIR}/config/config.yaml"
EXAMPLE_PATH="${ROOT_DIR}/config/config.local.publisher.example.yaml"
PGHOST_LOCAL="${PGHOST_LOCAL:-localhost}"
PGPORT_LOCAL="${PGPORT_LOCAL:-5432}"
PGUSER_LOCAL="${PGUSER_LOCAL:-postgres}"
PGPASSWORD_LOCAL="${PGPASSWORD_LOCAL:-postgres}"
DOCKER_PG_PORT="${DOCKER_PG_PORT:-5433}"
DOCKER_PROJECT_NAME="${DOCKER_PROJECT_NAME:-dp1-feed-v2-localpg}"
DOCKER_PG_CONTAINER="${DOCKER_PG_CONTAINER:-dp1-feed-v2-local-postgres}"
DOCKER_PG_VOLUME="${DOCKER_PG_VOLUME:-dp1-feed-v2-local-postgres-data}"
LOCAL_WEB_HOST="${LOCAL_WEB_HOST:-localhost}"
LOCAL_WEB_PORT="${LOCAL_WEB_PORT:-8787}"
LOCAL_WEB_URL="http://${LOCAL_WEB_HOST}:${LOCAL_WEB_PORT}"

cd "${ROOT_DIR}"

ensure_config() {
  if [[ ! -f "${CONFIG_PATH}" ]]; then
    cp "${EXAMPLE_PATH}" "${CONFIG_PATH}"
    echo "Created ${CONFIG_PATH} from local publisher example."
  fi

  if grep -q 'replace-me-with-openssl-rand-hex-32' "${CONFIG_PATH}"; then
    local generated_key
    generated_key="$(openssl rand -hex 32)"
    sed -i "s/replace-me-with-openssl-rand-hex-32/${generated_key}/" "${CONFIG_PATH}"
    echo "Generated a local development signing key in ${CONFIG_PATH}."
  fi
}

can_use_local_postgres() {
  command -v psql >/dev/null 2>&1 || return 1
  export PGPASSWORD="${PGPASSWORD_LOCAL}"

  psql \
    -h "${PGHOST_LOCAL}" \
    -p "${PGPORT_LOCAL}" \
    -U "${PGUSER_LOCAL}" \
    -d postgres \
    -tAc "SELECT 1" >/dev/null 2>&1 || return 1

  if psql \
    -h "${PGHOST_LOCAL}" \
    -p "${PGPORT_LOCAL}" \
    -U "${PGUSER_LOCAL}" \
    -d postgres \
    -tAc "SELECT 1 FROM pg_database WHERE datname='dp1_feed'" | grep -q 1; then
    return 0
  fi

  echo "Creating local database dp1_feed."
  createdb \
    -h "${PGHOST_LOCAL}" \
    -p "${PGPORT_LOCAL}" \
    -U "${PGUSER_LOCAL}" \
    dp1_feed >/dev/null 2>&1 || return 1

  return 0
}

start_docker_postgres() {
  local selected_port="${DOCKER_PG_PORT}"
  while ss -ltn "( sport = :${selected_port} )" | grep -q "${selected_port}"; do
    selected_port="$((selected_port + 1))"
  done

  echo "Starting isolated Docker Postgres on localhost:${selected_port}."
  docker rm -f "${DOCKER_PG_CONTAINER}" >/dev/null 2>&1 || true
  docker run -d \
    --name "${DOCKER_PG_CONTAINER}" \
    -e POSTGRES_USER=postgres \
    -e POSTGRES_PASSWORD=postgres \
    -e POSTGRES_DB=dp1_feed \
    -p "${selected_port}:5432" \
    -v "${DOCKER_PG_VOLUME}:/var/lib/postgresql/data" \
    postgres:18-alpine >/dev/null

  until PGPASSWORD=postgres psql \
    -h localhost \
    -p "${selected_port}" \
    -U postgres \
    -d dp1_feed \
    -tAc "SELECT 1" >/dev/null 2>&1; do
    sleep 1
  done

  export DP1_FEED_DATABASE_URL="postgres://postgres:postgres@localhost:${selected_port}/dp1_feed?sslmode=disable"
}

ensure_config

export DP1_FEED_SERVER_HOST="0.0.0.0"
export DP1_FEED_SERVER_PORT="${LOCAL_WEB_PORT}"
export DP1_FEED_PUBLISHER_AUTH_RP_ID="${LOCAL_WEB_HOST}"
export DP1_FEED_PUBLISHER_AUTH_RP_ORIGINS_JSON="[\"${LOCAL_WEB_URL}\"]"
export DP1_FEED_PUBLIC_BASE_URL="${LOCAL_WEB_URL}"

echo "Open the publisher at:"
echo "  ${LOCAL_WEB_URL}/publisher"
echo "Use localhost for local passkey testing."

if ss -ltn '( sport = :5432 )' | grep -q 5432; then
  echo "Postgres already appears to be listening on localhost:5432; trying to use it."
else
  if docker compose up -d postgres; then
    go run ./cmd/server -config "${CONFIG_PATH}"
    exit 0
  fi
fi

if can_use_local_postgres; then
  echo "Using local Postgres on localhost:${PGPORT_LOCAL}."
else
  echo "Local Postgres is not usable for this app; falling back to Docker."
  start_docker_postgres
fi

go run ./cmd/server -config "${CONFIG_PATH}"
