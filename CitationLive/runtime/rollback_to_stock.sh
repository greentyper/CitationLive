#!/bin/sh
# Archive the installed app and boot hook. A reboot clears its runtime overlays.
set -eu
umask 077

data=/data
sandbox=0
if [ "${1:-}" = --sandbox-root ]; then
    [ "$#" -ge 2 ] && [ ! -L "$2" ] || exit 2
    data=$(cd "$2" && pwd -P)
    shift 2
    [ "$data" != / ] && [ "$data" != /data ] && [ -f "$data/.citation-installer-sandbox" ] || exit 2
    sandbox=1
fi

action=${1:-}
hook_hash=${2:-}
case "$action" in probe|apply|verify) ;; *) exit 2 ;; esac
case "$hook_hash" in ''|*[!a-f0-9]*) exit 2 ;; esac
[ "${#hook_hash}" -eq 64 ] || exit 2

app=$data/opt/citation-gemini
hook=$data/alsa_looper.sh
backups=$data/opt/citationlive-stock-backups
pending=$backups/pending
units='citationlive-autoupdate.timer citationlive-autoupdate.service citation-gemini.service citation-spotify-name.service citation-spotify-home.service citation-spotify-connect.service citation-spotify-pcm.service'

fail() { printf '%s\n' "$1"; exit 1; }
for path in "$data" "$data/opt" "$app" "$hook" "$backups" "$pending"; do
    [ ! -L "$path" ] || fail 'unsafe-path'
done
if [ "$sandbox" = 0 ]; then
    [ "$(id -u)" -eq 0 ] && [ "$(uname -m)" = armv7l ] &&
        [ -f /chrome/bin/iot_audio_capture ] && grep -q '/data/alsa_looper.sh' /bin/alsa_looper.sh ||
        fail 'unsupported-speaker'
fi

stock_runtime() {
    if [ "$sandbox" = 1 ]; then return 0; fi
    for unit in $units; do
        if systemctl is-active --quiet "$unit"; then return 1; fi
    done
    systemctl is-active --quiet system-manager.service &&
        grep -Eq '[[:space:]]@/tmp/mixer-service$' /proc/net/unix &&
        awk '$5 == "/chrome/lib/libassistant.so" || $5 == "/chrome/process.json" ||
             $5 == "/chrome/bin/iot_audio_capture" || $5 == "/etc/asound-product.conf" { found=1 }
             END { exit found ? 1 : 0 }' /proc/self/mountinfo
}

check_installation() {
    if [ -e "$hook" ]; then
        [ -f "$hook" ] && [ "$(sha256sum "$hook" | cut -d ' ' -f 1)" = "$hook_hash" ] || fail 'unknown-boot-hook'
    fi
    if [ -e "$app" ]; then
        [ -d "$app" ] && [ ! -L "$app/bin" ] && [ -f "$app/bin/supervise.sh" ] &&
            [ ! -L "$app/bin/supervise.sh" ] &&
            grep -Fq '# /data/opt/citation-gemini/bin/supervise.sh' "$app/bin/supervise.sh" || fail 'unknown-installation'
    fi
}

if [ "$action" = verify ]; then
    [ ! -e "$app" ] && [ ! -e "$hook" ] && [ ! -e "$pending" ] && stock_runtime || fail 'stock-runtime-not-ready'
    echo stock
    exit 0
fi

check_installation
if [ "$action" = probe ]; then
    if [ -e "$app" ] || [ -e "$hook" ] || [ -d "$pending" ]; then
        echo installed
    elif stock_runtime; then
        echo stock
    else
        echo pending-reboot
    fi
    exit 0
fi

if [ ! -e "$app" ] && [ ! -e "$hook" ] && [ ! -e "$pending" ]; then
    echo pending-reboot
    exit 0
fi
mkdir -p "$backups"
chmod 700 "$backups"
command -v flock >/dev/null 2>&1 || fail 'rollback-lock-unavailable'
exec 9>"$backups/lock"
flock -n 9 || fail 'rollback-busy'
check_installation
mkdir -p "$pending"
if [ "$sandbox" = 0 ]; then
    for unit in $units; do
        if systemctl is-active --quiet "$unit"; then
            systemctl stop "$unit" || fail 'stop-failed'
        fi
    done
fi
check_installation
archive_path() {
    # The root-owned archive directory is private and guarded by the rollback lock.
    [ ! -e "$2" ] && [ ! -L "$2" ] || fail 'archive-conflict'
    mv -T "$1" "$2"
}
if [ -e "$hook" ]; then
    archive_path "$hook" "$pending/alsa_looper.sh"
fi
if [ -e "$app" ]; then
    archive_path "$app" "$pending/citation-gemini"
fi
archive=$(mktemp -d "$backups/stock.XXXXXXXX")
mv -T "$pending" "$archive"
sync
printf '%s\n' "$archive"
