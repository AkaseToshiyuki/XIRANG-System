#!/bin/sh
set -eu

delay=${1:-0}
case "${delay}" in
  ''|*[!0-9]*) echo "delay must be an integer number of seconds" >&2; exit 2 ;;
esac

printf 'rank=%s local_rank=%s world=%s node_rank=%s address=%s interface=%s\n' \
  "${XIRANG_RANK:?}" "${XIRANG_LOCAL_RANK:?}" "${XIRANG_WORLD_SIZE:?}" \
  "${XIRANG_NODE_RANK:?}" "${XIRANG_PRIMARY_ADDRESS:?}" "${XIRANG_NETWORK_INTERFACE:-default}"
sleep "${delay}"
printf 'rank=%s completed\n' "${XIRANG_RANK:?}"
