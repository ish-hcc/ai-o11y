#!/usr/bin/env bash
# Removes the ai-o11y collector that install.sh installed: stops and deletes
# the aio11y-otelcol service, its binary, configuration and disk queue.
#
#   sudo ./uninstall.sh
#   sudo --preserve-env=AIO11Y_AUTH_TOKEN ./uninstall.sh --deregister
#
# Options:
#   --deregister    also delete the target from ai-o11y (its stored series stay
#                   in the backend); nothing is removed when that fails
#   --server URL    ai-o11y address; the one saved at install when not given
#
# Environment:
#   AIO11Y_AUTH_TOKEN  ai-o11y's bearer token, when it requires one
set -euo pipefail

usage() { sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; }
die() { echo "uninstall.sh: $*" >&2; exit 1; }

server='' deregister=false
while (($#)); do
  case $1 in
    --server) server=${2:?--server needs a value}; shift ;;
    --deregister) deregister=true ;;
    -h | --help) usage; exit 0 ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
  shift
done

# AIO11Y_ROOT moves every installed path under another root, for tests.
root=${AIO11Y_ROOT:-/}
root=${root%/}
[[ -n $root || $(id -u) == 0 ]] || die "run as root"

etc_dir=$root/etc/aio11y-otelcol
id_file=$etc_dir/target-id
server_file=$etc_dir/server
bin_path=$root/usr/local/bin/aio11y-otelcol
state_dir=$root/var/lib/aio11y-otelcol
unit=aio11y-otelcol
unit_path=$root/etc/systemd/system/$unit.service

if $deregister; then
  [[ -f $id_file ]] || die "$id_file is missing: the target id is not known"
  id=$(<"$id_file")
  [[ $id =~ ^[A-Za-z0-9._-]+$ ]] || die "unexpected target id in $id_file: $id"
  [[ -n $server || ! -f $server_file ]] || server=$(<"$server_file")
  [[ -n $server ]] || die "--server is required"
  server=${server%/}

  umask 077
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  # The token goes to curl in a header file so it is not in curl's arguments.
  auth=()
  if [[ -n ${AIO11Y_AUTH_TOKEN:-} ]]; then
    printf 'Authorization: Bearer %s\n' "$AIO11Y_AUTH_TOKEN" >"$work/auth"
    auth=(-H "@$work/auth")
  fi
  code=$(curl --silent --show-error --fail --max-time 30 "${auth[@]}" -o /dev/null -w '%{http_code}' \
    -X DELETE "$server/aio11y/targets/$id") || true
  case $code in
    200) echo "target $id deleted from $server" ;;
    404) echo "target $id was not registered at $server" ;;
    *) die "deleting target $id from $server failed (HTTP ${code:-none}); nothing removed" ;;
  esac
fi

if [[ -f $unit_path ]]; then
  systemctl disable --now "$unit"
  rm -f "$unit_path"
  systemctl daemon-reload
fi
rm -f "$bin_path" "$etc_dir/config.yaml" "$etc_dir/env" "$id_file" "$server_file"
if [[ -d $etc_dir ]]; then
  rmdir "$etc_dir" || echo "warning: $etc_dir holds other files; left in place" >&2
fi
rm -rf "$state_dir"
echo "$unit removed"
