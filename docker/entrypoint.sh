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

# FOGPING_USER / FOGPING_PASSWD / FOGPING_PASSWD_FILE are read by fogping itself
# from the environment. They are not turned into user=/passwd= arguments here:
# a process's arguments are readable by everyone on the host (ps), its
# environment is not.

exec fogping "$@"
