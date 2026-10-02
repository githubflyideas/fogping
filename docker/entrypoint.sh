#!/bin/sh
# Map FOGPING_* env vars to flags, for runners that can set env but not a command
# (Docker Desktop, NAS container managers, RouterOS /container).
# Command-line args are kept and come last, so they win over env on conflict.
set -e

case "${FOGPING_EDIT:-}" in
  1|true|TRUE|yes|on) set -- --edit "$@" ;;
esac

if [ -n "${FOGPING_DAYS:-}" ]; then
  set -- "--days=${FOGPING_DAYS}" "$@"
fi

if [ -n "${FOGPING_USER:-}" ] || [ -n "${FOGPING_PASSWD:-}" ]; then
  set -- "user=${FOGPING_USER:-}" "passwd=${FOGPING_PASSWD:-}" "$@"
fi

exec fogping "$@"
