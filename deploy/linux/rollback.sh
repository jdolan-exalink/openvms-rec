#!/usr/bin/env bash
# Restore the binary kept by install.sh. Database migrations are forward-only:
# roll back only to a version with the same schema, or restore a backup.
set -euo pipefail
BIN=/usr/local/bin/openvms-recorder
[[ -f $BIN.prev ]] || { echo "no previous binary" >&2; exit 1; }
systemctl stop openvms-recorder
mv -f "$BIN" "$BIN.rolledback" && mv -f "$BIN.prev" "$BIN"
systemctl start openvms-recorder
"$BIN" version
