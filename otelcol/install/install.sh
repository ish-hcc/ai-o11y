#!/usr/bin/env bash
# Installs the ai-o11y collector on a Linux VM as the systemd service
# aio11y-otelcol: registers the VM as a target, fetches its collector
# configuration, checks it with the collector and starts the service.
#
#   sudo --preserve-env=AIO11Y_AUTH_TOKEN,AIO11Y_WRITE_PASSWORD ./install.sh \
#     --server http://ai-o11y:8094 --ns ns01 --infra infra01 --node node01 \
#     --binary ./aio11y-otelcol          (or --binary-url <URL>)
#
#   sudo --preserve-env=AIO11Y_AUTH_TOKEN ./install.sh --config-only
#                                        fetch the configuration again, restart
#
# Options:
#   --server URL        ai-o11y address; on a reinstall or --config-only the
#                       one saved at install is used when not given
#   --ns, --infra, --node
#                       the VM's CB-Tumblebug nsId, infraId and nodeId
#   --binary PATH       collector binary to install
#   --binary-url URL    collector binary to download
#                       (neither: keep the installed binary)
#   --config-only       fetch the configuration of the installed target and
#                       restart; no registration, binary or unit change
#
# Environment:
#   AIO11Y_AUTH_TOKEN      ai-o11y's bearer token, when it requires one
#   AIO11Y_WRITE_PASSWORD  the backend write account's password, when the
#                          backend uses basic auth; kept from the last install
#                          when not given
#
# Secrets are read from the environment only, so they never show in the
# process list. Installing again registers the VM again, which replaces its
# description, and replaces the binary, configuration and unit.
set -euo pipefail

usage() { sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'; }
die() { echo "install.sh: $*" >&2; exit 1; }

server='' ns='' infra='' node='' binary='' binary_url='' config_only=false
while (($#)); do
  case $1 in
    --server) server=${2:?--server needs a value}; shift ;;
    --ns) ns=${2:?--ns needs a value}; shift ;;
    --infra) infra=${2:?--infra needs a value}; shift ;;
    --node) node=${2:?--node needs a value}; shift ;;
    --binary) binary=${2:?--binary needs a value}; shift ;;
    --binary-url) binary_url=${2:?--binary-url needs a value}; shift ;;
    --config-only) config_only=true ;;
    -h | --help) usage; exit 0 ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
  shift
done

# AIO11Y_ROOT moves every installed path under another root, for tests.
root=${AIO11Y_ROOT:-/}
root=${root%/}
[[ -n $root || $(id -u) == 0 ]] || die "run as root"

here=$(cd "$(dirname "$0")" && pwd)
etc_dir=$root/etc/aio11y-otelcol
config=$etc_dir/config.yaml
env_file=$etc_dir/env
id_file=$etc_dir/target-id
server_file=$etc_dir/server
bin_path=$root/usr/local/bin/aio11y-otelcol
unit=aio11y-otelcol
unit_path=$root/etc/systemd/system/$unit.service
# AIO11Y_SETTLE_SECONDS is how long a restarted collector runs before it must
# still be active, so one that exits on its configuration is caught; tests set 0.
settle=${AIO11Y_SETTLE_SECONDS:-5}
ident='^[A-Za-z0-9._-]+$'

[[ -n $binary && -n $binary_url ]] && die "give --binary or --binary-url, not both"
if $config_only; then
  [[ -f $unit_path && -f $id_file ]] || die "not installed: run install.sh without --config-only first"
  [[ -z $binary && -z $binary_url ]] || die "--config-only keeps the installed binary"
else
  for v in ns infra node; do
    [[ -n ${!v} ]] || die "--$v is required"
    [[ ${!v} =~ $ident ]] || die "--$v may hold letters, digits, '.', '_' and '-' only"
  done
  [[ -f $here/$unit.service ]] || die "$here/$unit.service is missing: run install.sh from its directory"
  [[ -n $binary || -n $binary_url || -x $bin_path ]] || die "--binary or --binary-url is required"
  [[ -z $binary || -f $binary ]] || die "$binary is not a file"
fi
[[ -n $server || ! -f $server_file ]] || server=$(<"$server_file")
[[ -n $server ]] || die "--server is required"
server=${server%/}
if [[ -n ${AIO11Y_WRITE_PASSWORD:-} && $AIO11Y_WRITE_PASSWORD == *$'\n'* ]]; then
  die "AIO11Y_WRITE_PASSWORD must be one line"
fi

umask 077
work=$(mktemp -d)
staged_bin=$bin_path.new
staged_config=$config.new
trap 'rm -rf "$work"; rm -f "$staged_bin" "$staged_config" "$env_file.new"' EXIT

# The token goes to curl in a header file so it is not in curl's arguments.
auth=()
if [[ -n ${AIO11Y_AUTH_TOKEN:-} ]]; then
  printf 'Authorization: Bearer %s\n' "$AIO11Y_AUTH_TOKEN" >"$work/auth"
  auth=(-H "@$work/auth")
fi
api() { curl --silent --show-error --fail --max-time 30 "${auth[@]}" "$@"; }

# 1. Register. A target registered before is registered again and keeps its
# id, the CB-Tumblebug node Uid.
if $config_only; then
  id=$(<"$id_file")
else
  body=$(printf '{"kind":"node","nsId":"%s","infraId":"%s","nodeId":"%s"}' "$ns" "$infra" "$node")
  resp=$(api -X POST -H 'Content-Type: application/json' --data-binary "$body" "$server/aio11y/targets") ||
    die "registering $ns/$infra/$node with $server failed"
  [[ $resp =~ \"id\":\"([^\"]*)\" ]] || die "registration answered no target id"
  id=${BASH_REMATCH[1]}
fi
[[ $id =~ $ident ]] || die "unexpected target id: $id"
echo "target $id"

# 2. Stage the binary and the configuration next to where they go, and check
# the configuration with the binary that will run it.
mkdir -p "$etc_dir" "$(dirname "$bin_path")" "$(dirname "$unit_path")"
chmod 0755 "$etc_dir"
run_bin=$bin_path
if [[ -n $binary ]]; then
  cp "$binary" "$staged_bin"
  run_bin=$staged_bin
elif [[ -n $binary_url ]]; then
  curl --silent --show-error --fail --location --max-time 600 -o "$staged_bin" "$binary_url" ||
    die "downloading $binary_url failed"
  run_bin=$staged_bin
fi
chmod 0755 "$run_bin"
api -o "$staged_config" "$server/aio11y/targets/$id/collectorConfig" ||
  die "fetching the collector configuration of $id failed"
chmod 0640 "$staged_config"
"$run_bin" validate --config "$staged_config" >&2 ||
  die "the collector configuration of $id does not validate; nothing installed"

# 3. Install. Renames, so a running collector never reads a half-written file.
if [[ $run_bin == "$staged_bin" ]]; then
  mv -f "$staged_bin" "$bin_path"
fi
mv -f "$staged_config" "$config"
if [[ -n ${AIO11Y_WRITE_PASSWORD:-} ]]; then
  # systemd's EnvironmentFile reads a double-quoted value with \\ \" \$ \`
  # escaped.
  v=${AIO11Y_WRITE_PASSWORD//\\/\\\\}
  v=${v//\"/\\\"}
  v=${v//\$/\\\$}
  v=${v//\`/\\\`}
  printf 'AIO11Y_WRITE_PASSWORD="%s"\n' "$v" >"$env_file.new"
  chmod 0600 "$env_file.new"
  mv -f "$env_file.new" "$env_file"
  unset v
elif [[ ! -f $env_file ]] && grep -q 'env:AIO11Y_WRITE_PASSWORD' "$config"; then
  echo "warning: the configuration signs writes with AIO11Y_WRITE_PASSWORD, which is not set;" \
    "every write will be refused. Run again with AIO11Y_WRITE_PASSWORD." >&2
fi
if ! $config_only; then
  printf '%s\n' "$id" >"$id_file"
  printf '%s\n' "$server" >"$server_file"
  chmod 0644 "$id_file" "$server_file"
  install -m 0644 "$here/$unit.service" "$unit_path"
  systemctl daemon-reload
  systemctl enable "$unit"
fi

# 4. Start, or restart a running collector to read what changed.
systemctl restart "$unit"
sleep "$settle"
if ! systemctl is-active --quiet "$unit"; then
  journalctl -u "$unit" -n 30 --no-pager >&2 || true
  die "$unit is not active"
fi
journalctl -u "$unit" -n 10 --no-pager || true
echo "$unit is active (target $id)"
