#!/bin/sh
# dl-tool container entrypoint. Runs as root, drops to PUID:PGID, execs the binary.
# Order is fixed by docs/10-deployment-and-compose.md section 4.
set -eu

PUID="${PUID:-1000}"; PGID="${PGID:-1000}"
UMASK="${UMASK:-002}"; TZ="${TZ:-Etc/UTC}"

# /etc is only writable as root; under compose `user:` the zone reaches the
# Go process through the TZ environment variable instead (doc 11 section 3).
if [ "$(id -u)" -eq 0 ] && [ -f "/usr/share/zoneinfo/$TZ" ]; then
  ln -snf "/usr/share/zoneinfo/$TZ" /etc/localtime
  printf '%s\n' "$TZ" > /etc/timezone
fi

umask "$UMASK"

if [ "$(id -u)" -ne 0 ]; then
  echo "entrypoint: already running as $(id -u):$(id -g); skipping user creation and chown" >&2
  exec /usr/local/bin/dl-tool "$@"
fi

if [ "$PUID" = "0" ]; then
  echo "entrypoint: PUID=0, running dl-tool as root" >&2
  exec /usr/local/bin/dl-tool "$@"
fi

getent group  "$PGID" >/dev/null 2>&1 || addgroup -g "$PGID" dltool
getent passwd "$PUID" >/dev/null 2>&1 || \
  adduser -D -H -u "$PUID" -G "$(getent group "$PGID" | cut -d: -f1)" dltool

mkdir -p "${DLTOOL_CONFIG_DIR:-/config}"
chown -R "$PUID:$PGID" "${DLTOOL_CONFIG_DIR:-/config}"
# NEVER chown /data recursively: it can hold terabytes and the operator owns its permissions.

oldIFS=$IFS; IFS=:
for root in ${DLTOOL_DATA_ROOTS:-/data}; do
  su-exec "$PUID:$PGID" test -w "$root" || \
    echo "entrypoint: data_root_not_writable $root as $PUID:$PGID" >&2
done
IFS=$oldIFS

exec su-exec "$PUID:$PGID" /usr/local/bin/dl-tool "$@"
