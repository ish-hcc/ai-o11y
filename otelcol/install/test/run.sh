#!/usr/bin/env bash
# Runs install.sh and uninstall.sh end to end without root: a fake ai-o11y
# (fake_server.py), fake systemctl and journalctl first on PATH, a fake
# collector binary, and every installed path under a scratch AIO11Y_ROOT.
#
#   ./run.sh        exit 0 when every check passes
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
scripts=$(dirname "$here")
command -v python3 >/dev/null || { echo "run.sh: python3 is required" >&2; exit 1; }

tmp=$(mktemp -d)
server_pid=''
cleanup() {
  [[ -z $server_pid ]] || kill "$server_pid" 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

export FAKE_LOG=$tmp/log FAKE_TOKEN=test-token-123 FAKE_CONFIG=$tmp/served.yaml
export REAL_CURL
REAL_CURL=$(command -v curl)
export AIO11Y_ROOT=$tmp/root AIO11Y_SETTLE_SECONDS=0
export PATH=$here/fakebin:$PATH
mkdir -p "$FAKE_LOG" "$AIO11Y_ROOT"

cat >"$FAKE_CONFIG" <<'EOF'
exporters:
  otlphttp/0:
    auth:
      authenticator: basicauth/0
extensions:
  basicauth/0:
    client_auth:
      password: ${env:AIO11Y_WRITE_PASSWORD}
      username: writer
EOF
sed 's/FAKE_BUILD/local/' "$here/fake-otelcol" >"$tmp/otelcol-local"
sed 's/FAKE_BUILD/download/' "$here/fake-otelcol" >"$tmp/otelcol-download"
chmod 0755 "$tmp/otelcol-local" "$tmp/otelcol-download"

python3 -I "$here/fake_server.py" "$tmp/port" &
server_pid=$!
for _ in $(seq 50); do
  [[ -f $tmp/port ]] && break
  sleep 0.1
done
[[ -f $tmp/port ]] || { echo "run.sh: fake server did not start" >&2; exit 1; }
url=http://localhost:$(<"$tmp/port")

etc=$AIO11Y_ROOT/etc/aio11y-otelcol
bin=$AIO11Y_ROOT/usr/local/bin/aio11y-otelcol
unit=$AIO11Y_ROOT/etc/systemd/system/aio11y-otelcol.service
# A password systemd would read wrongly without the escapes.
password='p"a\s$s`w o#rd'"'"'x'

failed=0
check() { # <what> <command...>
  local what=$1
  shift
  if "$@"; then
    echo "ok    $what"
  else
    echo "FAIL  $what" >&2
    failed=1
  fi
}
mode() { [[ $(stat -c %a "$1") == "$2" ]]; }
has() { grep -qF -- "$2" "$1"; }
lacks() { [[ -f $1 ]] && ! grep -qF -- "$2" "$1"; }
absent() { [[ ! -e $1 ]]; }
present() { [[ -e $1 ]]; }
empty() { [[ ! -s $1 ]]; }
count() { [[ $(grep -cF -- "$2" "$1") == "$3" ]]; }
# Runs a script with its output kept in $tmp/out; answers its exit status.
run() {
  local rc=0
  "$@" >"$tmp/out" 2>&1 || rc=$?
  return "$rc"
}
fails() { ! run "$@"; }
reset_logs() { rm -f "$FAKE_LOG/systemctl.log" "$FAKE_LOG/curl.log" "$FAKE_LOG/server.log"; }

echo "-- install"
check "install succeeds" run env AIO11Y_AUTH_TOKEN=$FAKE_TOKEN AIO11Y_WRITE_PASSWORD="$password" \
  "$scripts/install.sh" --server "$url/" --ns ns01 --infra infra01 --node node01 --binary "$tmp/otelcol-local"
check "registers kind node by nsId/infraId/nodeId with the token" \
  has "$FAKE_LOG/server.log" "POST /aio11y/targets auth=Bearer $FAKE_TOKEN {\"kind\":\"node\",\"nsId\":\"ns01\",\"infraId\":\"infra01\",\"nodeId\":\"node01\"}"
check "fetches the configuration of the answered id" \
  has "$FAKE_LOG/server.log" "GET /aio11y/targets/uid-node01/collectorConfig auth=Bearer $FAKE_TOKEN"
check "config.yaml is the served configuration" cmp -s "$FAKE_CONFIG" "$etc/config.yaml"
check "config.yaml is 0640" mode "$etc/config.yaml" 640
check "env is 0600" mode "$etc/env" 600
check "env holds the escaped password" \
  has "$etc/env" 'AIO11Y_WRITE_PASSWORD="p\"a\\s\$s\`w o#rd'"'"'x"'
check "target-id is the answered id" [ "$(<"$etc/target-id")" == uid-node01 ]
check "server is saved without the trailing slash" [ "$(<"$etc/server")" == "$url" ]
check "binary is the given one" has "$bin" "build: local"
check "binary is 0755" mode "$bin" 755
check "unit is installed" cmp -s "$scripts/aio11y-otelcol.service" "$unit"
check "configuration validated before install" has "$FAKE_LOG/otelcol.log" "validate $etc/config.yaml.new"
check "systemctl: daemon-reload, enable, restart, is-active in order" \
  diff <(printf '%s\n' daemon-reload 'enable aio11y-otelcol' 'restart aio11y-otelcol' 'is-active --quiet aio11y-otelcol') \
  "$FAKE_LOG/systemctl.log"
check "every curl call has --fail and --max-time" \
  bash -c '! grep -v -e "--fail.*--max-time" "$1"' _ "$FAKE_LOG/curl.log"
check "the token is not in curl's arguments" lacks "$FAKE_LOG/curl.log" "$FAKE_TOKEN"
check "the password is not printed" lacks "$tmp/out" 'w o#rd'
check "reports active" has "$tmp/out" "aio11y-otelcol is active (target uid-node01)"
check "no staged file left" bash -c '! ls "$1"/*.new "$2".new 2>/dev/null' _ "$etc" "$bin"

echo "-- reinstall from a URL, password not given"
reset_logs
env_before=$(<"$etc/env")
check "reinstall succeeds" run env AIO11Y_AUTH_TOKEN=$FAKE_TOKEN \
  "$scripts/install.sh" --server "$url" --ns ns01 --infra infra01 --node node01 --binary-url "file://$tmp/otelcol-download"
check "registers again" count "$FAKE_LOG/server.log" "POST /aio11y/targets" 1
check "binary is the downloaded one" has "$bin" "build: download"
check "env is kept" [ "$(<"$etc/env")" == "$env_before" ]
check "restarts the running collector" has "$FAKE_LOG/systemctl.log" "restart aio11y-otelcol"

echo "-- reinstall keeping the installed binary, server from the saved file"
reset_logs
check "reinstall without --binary succeeds" run env AIO11Y_AUTH_TOKEN=$FAKE_TOKEN \
  "$scripts/install.sh" --ns ns01 --infra infra01 --node node01
check "uses the saved server" has "$FAKE_LOG/server.log" "POST /aio11y/targets"
check "binary is still the downloaded one" has "$bin" "build: download"

echo "-- config-only"
reset_logs
printf '# second\n' >>"$FAKE_CONFIG"
check "config-only succeeds" run env AIO11Y_AUTH_TOKEN=$FAKE_TOKEN "$scripts/install.sh" --config-only
check "does not register" lacks "$FAKE_LOG/server.log" "POST"
check "fetches the configuration again" cmp -s "$FAKE_CONFIG" "$etc/config.yaml"
check "only restarts" \
  diff <(printf '%s\n' 'restart aio11y-otelcol' 'is-active --quiet aio11y-otelcol') "$FAKE_LOG/systemctl.log"

echo "-- config-only with a configuration that does not validate"
reset_logs
cp "$etc/config.yaml" "$tmp/config.before"
printf '# INVALID\n' >>"$FAKE_CONFIG"
check "config-only fails" fails env AIO11Y_AUTH_TOKEN=$FAKE_TOKEN "$scripts/install.sh" --config-only
check "says nothing was installed" has "$tmp/out" "does not validate; nothing installed"
check "config.yaml is unchanged" cmp -s "$tmp/config.before" "$etc/config.yaml"
check "the service is not touched" empty "$FAKE_LOG/systemctl.log"
check "no staged config left" absent "$etc/config.yaml.new"
sed -i '/INVALID/d' "$FAKE_CONFIG"

echo "-- refusals"
reset_logs
check "wrong token fails" fails env AIO11Y_AUTH_TOKEN=wrong \
  "$scripts/install.sh" --server "$url" --ns ns01 --infra infra01 --node node02 --binary "$tmp/otelcol-local"
check "wrong token: says registration failed" has "$tmp/out" "registering ns01/infra01/node02"
check "wrong token: binary is not replaced" has "$bin" "build: download"
check "an id with a quote is refused" fails \
  "$scripts/install.sh" --server "$url" --ns 'ns"01' --infra infra01 --node node01
check "a service that does not stay up fails" fails env AIO11Y_AUTH_TOKEN=$FAKE_TOKEN FAKE_ACTIVE=failed \
  "$scripts/install.sh" --config-only
check "a service that does not stay up: shows its log" has "$tmp/out" "fake journal: -u aio11y-otelcol -n 30"

echo "-- uninstall --deregister"
reset_logs
mkdir -p "$AIO11Y_ROOT/var/lib/aio11y-otelcol"
touch "$AIO11Y_ROOT/var/lib/aio11y-otelcol/queue"
check "uninstall succeeds" run env AIO11Y_AUTH_TOKEN=$FAKE_TOKEN "$scripts/uninstall.sh" --deregister
check "deletes the target" has "$FAKE_LOG/server.log" "DELETE /aio11y/targets/uid-node01 auth=Bearer $FAKE_TOKEN"
check "systemctl: disable --now, daemon-reload" \
  diff <(printf '%s\n' 'disable --now aio11y-otelcol' daemon-reload) "$FAKE_LOG/systemctl.log"
check "removes the configuration directory" absent "$etc"
check "removes the binary" absent "$bin"
check "removes the unit" absent "$unit"
check "removes the state directory" absent "$AIO11Y_ROOT/var/lib/aio11y-otelcol"
check "the token is not in curl's arguments" lacks "$FAKE_LOG/curl.log" "$FAKE_TOKEN"

echo "-- uninstall of a target ai-o11y no longer has"
check "install again" run env AIO11Y_AUTH_TOKEN=$FAKE_TOKEN \
  "$scripts/install.sh" --server "$url" --ns ns01 --infra infra01 --node node03 --binary "$tmp/otelcol-local"
echo gone >"$etc/target-id"
check "uninstall --deregister succeeds on 404" run env AIO11Y_AUTH_TOKEN=$FAKE_TOKEN "$scripts/uninstall.sh" --deregister
check "says it was not registered" has "$tmp/out" "target gone was not registered"
check "removes the binary" absent "$bin"

echo "-- uninstall --deregister when ai-o11y refuses"
check "install again" run env AIO11Y_AUTH_TOKEN=$FAKE_TOKEN \
  "$scripts/install.sh" --server "$url" --ns ns01 --infra infra01 --node node04 --binary "$tmp/otelcol-local"
check "wrong token fails" fails env AIO11Y_AUTH_TOKEN=wrong "$scripts/uninstall.sh" --deregister
check "says nothing was removed" has "$tmp/out" "(HTTP 401); nothing removed"
check "binary is kept" present "$bin"
check "plain uninstall succeeds" run "$scripts/uninstall.sh"
check "removes the configuration directory" absent "$etc"

if ((failed)); then
  echo "install tests FAILED" >&2
  exit 1
fi
echo "install tests passed"
