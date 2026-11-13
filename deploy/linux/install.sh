#!/usr/bin/env bash
# Install or upgrade OpenVMS Recorder on Debian 13 / Ubuntu 24.04.
#   sudo ./install.sh ./openvms-recorder
# Keeps the previous binary as openvms-recorder.prev for rollback.sh.
# Never touches recordings or the database.
set -euo pipefail

BIN_SRC=${1:?usage: install.sh PATH_TO_BINARY}
BIN=/usr/local/bin/openvms-recorder
ETC=/etc/openvms-recorder
DATA=/var/lib/openvms-recorder
USER_NAME=openvms-recorder
HERE=$(cd "$(dirname "$0")" && pwd)

[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 1; }

if ! command -v ffmpeg >/dev/null; then
  apt-get update -qq && apt-get install -y -qq ffmpeg
fi

id -u "$USER_NAME" >/dev/null 2>&1 || useradd --system --home "$DATA" --shell /usr/sbin/nologin "$USER_NAME"
install -d -m 0750 -o "$USER_NAME" -g "$USER_NAME" "$DATA"
install -d -m 0750 -o root -g "$USER_NAME" "$ETC"

"$BIN_SRC" version >/dev/null   # refuse a broken binary before swapping
if [[ -f $BIN ]]; then cp -p "$BIN" "$BIN.prev"; fi
install -m 0755 "$BIN_SRC" "$BIN.new" && mv -f "$BIN.new" "$BIN"

if [[ ! -f $ETC/recorder.yaml ]]; then
  install -m 0640 -o root -g "$USER_NAME" "$HERE/../../configs/recorder.example.yaml" "$ETC/recorder.yaml"
  sed -i "s#secrets_file: .*#secrets_file: $ETC/secrets.yaml#" "$ETC/recorder.yaml"
  echo "Edit $ETC/recorder.yaml (volumes, cameras) before starting."
fi
for f in secrets.yaml api_keys; do
  [[ -f $ETC/$f ]] || install -m 0600 -o "$USER_NAME" -g "$USER_NAME" /dev/null "$ETC/$f"
done

install -m 0644 "$HERE/../systemd/openvms-recorder.service" /etc/systemd/system/openvms-recorder.service
systemctl daemon-reload
systemctl enable openvms-recorder >/dev/null

cat <<MSG
Installed $("$BIN" version).
Next:
  1. For each recording root:  install -d -o $USER_NAME -g $USER_NAME -m 0750 /srv/cctv-d/recordings
  2. sudo -u $USER_NAME $BIN storage init -config $ETC/recorder.yaml <volume-id>
  3. systemctl restart openvms-recorder && journalctl -u openvms-recorder -f
MSG
