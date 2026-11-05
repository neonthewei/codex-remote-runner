#!/bin/sh
set -eu
script=$0
while [ -L "$script" ]; do
  target=$(readlink "$script")
  case "$target" in /*) script=$target ;; *) script=$(dirname "$script")/$target ;; esac
done
directory=$(CDPATH= cd -P -- "$(dirname -- "$script")" && pwd -P)
root=$(CDPATH= cd -- "$directory/../../.." && pwd -P)
if [ ! -x "$root/bin/codex-remote" ]; then
  echo 'Build the trusted checkout first: go build -o bin/codex-remote .' >&2
  exit 1
fi
exec "$root/bin/codex-remote" "$@"
