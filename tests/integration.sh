#!/usr/bin/env bash
set -euo pipefail
# Starts a disposable PostgreSQL that only accepts mTLS, then runs the PostgreSQL tests against it.
# The benchmark role cannot create databases.
fixture=$(mktemp -d)
container="mini-pgbench-test-$$"
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  rm -rf "$fixture"
}
trap cleanup EXIT

openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=test-ca \
  -keyout "$fixture/ca.key" -out "$fixture/ca.crt" 2>/dev/null
printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\n' > "$fixture/extensions"
for name in server client; do
  if [ "$name" = client ]; then cn=benchmark; else cn=localhost; fi
  openssl req -newkey rsa:2048 -nodes -subj "/CN=$cn" \
    -keyout "$fixture/$name.key" -out "$fixture/$name.csr" 2>/dev/null
  openssl x509 -req -in "$fixture/$name.csr" -CA "$fixture/ca.crt" -CAkey "$fixture/ca.key" \
    -CAcreateserial -days 2 -extfile "$fixture/extensions" -out "$fixture/$name.crt" 2>/dev/null
done
chmod 755 "$fixture"
chmod 644 "$fixture"/*.crt "$fixture/server.key"
printf 'local all all trust\nhostssl all all all cert\n' > "$fixture/pg_hba.conf"
printf 'CREATE ROLE benchmark LOGIN NOCREATEDB;\nCREATE DATABASE benchmark OWNER benchmark;\n' > "$fixture/init.sql"

docker run -d --name "$container" -e POSTGRES_HOST_AUTH_METHOD=trust -p 127.0.0.1::5432 \
  -v "$fixture:/fixtures:ro" -v "$fixture/init.sql:/docker-entrypoint-initdb.d/init.sql:ro" \
  postgres:18.6 bash -c '
    cp /fixtures/server.key /tmp/server.key
    chown postgres:postgres /tmp/server.key
    chmod 600 /tmp/server.key
    exec docker-entrypoint.sh postgres -c ssl=on \
      -c ssl_cert_file=/fixtures/server.crt -c ssl_key_file=/tmp/server.key \
      -c ssl_ca_file=/fixtures/ca.crt -c hba_file=/fixtures/pg_hba.conf' >/dev/null

ready=false
for _ in {1..60}; do
  if docker exec "$container" pg_isready -U postgres >/dev/null 2>&1 &&
    docker logs "$container" 2>&1 | grep -q 'PostgreSQL init process complete'; then
    ready=true
    break
  fi
  sleep 1
done
if [ "$ready" != true ]; then
  docker logs "$container"
  exit 1
fi

port=$(docker port "$container" 5432/tcp)
export TEST_PG_PORT=${port##*:}
export TEST_PG_TLS="$fixture"
go test -race -count=1 -run TestPostgres ./...
