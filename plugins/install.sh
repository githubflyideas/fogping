#!/usr/bin/env bash
# Install official probers into the directory fogping reads with --probes.
#
#   ./install.sh [-d DIR] NAME...     install the named probers (default DIR: ./probes)
#   ./install.sh [-d DIR] all         install every prober in this directory
#
# A prober is installed as DIR/NAME/manifest.json and DIR/NAME/probe, which is the
# whole layout fogping needs (docs/PROBE-PROTOCOL.md, section 8). Probers written
# in a compiled language have a Makefile, and `make` builds the `probe` file first.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
dest=./probes
if [[ ${1:-} == -d ]]; then dest=$2; shift 2; fi
[[ $# -ge 1 ]] || { sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

names=("$@")
if [[ ${names[0]} == all ]]; then
  names=()
  for d in "$here"/*/; do [[ -f $d/manifest.json ]] && names+=("$(basename "$d")"); done
fi

for name in "${names[@]}"; do
  src=$here/$name
  # The name becomes part of a path: allow only what a prober type can contain.
  [[ $name =~ ^[a-z][a-z0-9_-]{0,31}$ && -f $src/manifest.json ]] || { echo "no such prober: $name" >&2; exit 1; }
  if [[ -f $src/Makefile ]]; then
    make -C "$src" >&2
  fi
  [[ -x $src/probe ]] || { echo "$name: no executable 'probe' (did the build fail?)" >&2; exit 1; }
  # fogping refuses a probers directory that group or others can write to (whoever
  # can write there can run code as fogping), so do not inherit a loose umask.
  [[ -d $dest ]] || { mkdir -p "$dest"; chmod 0755 "$dest"; }
  mkdir -p "$dest/$name"
  chmod 0755 "$dest/$name"
  # `install` sets the mode explicitly: fogping refuses a probe file that is
  # writable by anyone but its owner.
  install -m 0644 "$src/manifest.json" "$dest/$name/manifest.json"
  install -m 0755 "$src/probe" "$dest/$name/probe"
  echo "installed $name -> $dest/$name"
done
