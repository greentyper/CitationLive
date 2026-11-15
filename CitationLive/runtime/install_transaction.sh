#!/bin/sh
# File-only fresh installation transaction. NEVER flashes MTD or reboots.
# Production activation is deliberately deferred until an explicit owner reboot.
set -eu
umask 077

data=/data
sandbox=0
if [ "${1:-}" = "--sandbox-root" ]; then
    sandbox=1
    data=${2:-}
    shift 2
    case "$data" in /*) ;; *) echo 'Invalid sandbox' >&2; exit 2 ;; esac
    [ ! -L "$data" ] || exit 2
    data=$(cd "$data" && pwd -P) || exit 2
    [ "$data" != /data ] && [ "$data" != / ] && [ -f "$data/.citation-installer-sandbox" ] || exit 2
fi
operation=${1:-}
id=${2:-}
expected=${3:-}
case "$operation" in apply|recover|status) ;; *) exit 2 ;; esac
case "$id" in ''|*[!a-f0-9]*) exit 2 ;; esac
[ "${#id}" -eq 32 ] || exit 2
if [ "$operation" = apply ]; then
    case "$expected" in ''|*[!a-f0-9]*) exit 2 ;; esac
    [ "${#expected}" -eq 64 ] || exit 2
fi

app="$data/opt/citation-gemini"
manager="$data/opt/citation-installer"
tx="$manager/transactions/$id"
hook="$data/alsa_looper.sh"
incoming="$manager/incoming/$id/payload.tar"
hash() { sha256sum "$1" | cut -d ' ' -f 1; }
die() { echo 'Transaction refused; no unrelated files will be overwritten.' >&2; exit 1; }
for directory in "$data" "$data/opt" "$manager" "$manager/incoming" "$manager/transactions"; do
    [ ! -L "$directory" ] || die
done
[ ! -L "$hook" ] && [ ! -L "$app" ] && [ ! -L "$tx" ] || die
if [ "$sandbox" = 0 ]; then
    [ "$(id -u)" = 0 ] && [ "$(uname -m)" = armv7l ] || die
fi

if [ "$operation" = status ]; then
    [ -f "$tx/phase" ] || die
    case "$(cat "$tx/phase")" in prepared|promoting|installed-pending-reboot|rolling-back|rolled-back)
        cat "$tx/phase" ;; *) die ;; esac
    exit 0
fi
mkdir -p "$manager/transactions"
chmod 700 "$manager" "$manager/transactions"
lockdir=
if command -v flock >/dev/null 2>&1; then
    exec 9>"$manager/transaction.lock"
    flock -n 9 || die
elif [ "$sandbox" = 1 ]; then
    # Test-host fallback only. Actual device installation requires flock.
    lockdir="$manager/.sandbox-lock"
    if ! mkdir "$lockdir" 2>/dev/null; then
        old=$(cat "$lockdir/pid" 2>/dev/null || true)
        case "$old" in ''|*[!0-9]*) die ;; esac
        if kill -0 "$old" 2>/dev/null; then die; fi
        rm -f "$lockdir/pid"
        rmdir "$lockdir" || die
        mkdir "$lockdir" || die
    fi
    echo $$ >"$lockdir/pid"
else
    die
fi

phase() {
    printf '%s\n' "$1" >"$tx/phase.next" || return 1
    mv "$tx/phase.next" "$tx/phase" || return 1
    sync || return 1
}
owned_app() {
    [ -d "$app" ] && [ ! -L "$app/.release-owner" ] && [ ! -L "$app/.transaction-id" ] &&
    [ "$(cat "$app/.release-owner" 2>/dev/null)" = citation-source-installer-v1 ] &&
    [ "$(cat "$app/.transaction-id" 2>/dev/null)" = "$id" ]
}
recover() {
    [ -f "$tx/phase" ] && [ -f "$tx/hook-sha256" ] || return 1
    if [ "$(cat "$tx/phase")" = rolled-back ]; then
        # Idempotence must not imply that a later unrelated install was undone.
        [ ! -e "$app" ] && [ ! -e "$hook" ] || return 1
        return 0
    fi
    case "$(cat "$tx/phase")" in prepared|promoting|installed-pending-reboot|rolling-back) ;; *) return 1 ;; esac
    [ "$(cat "$tx/original-app" 2>/dev/null)" = absent ] &&
    [ "$(cat "$tx/original-hook" 2>/dev/null)" = absent ] || return 1
    # Validate BOTH protected destinations before changing either of them.
    if [ -e "$app" ]; then owned_app || return 1; fi
    if [ -e "$hook" ]; then
        [ ! -L "$hook" ] && [ "$(hash "$hook")" = "$(cat "$tx/hook-sha256")" ] || return 1
    fi
    if [ -e "$tx/preserved-app" ] && [ -e "$app" ]; then return 1; fi
    if [ -e "$tx/preserved-hook" ] && [ -e "$hook" ]; then return 1; fi
    if [ "$sandbox" = 0 ] && [ -e "$app" ]; then
        [ ! -L "$app/disabled" ] || return 1
        touch "$app/disabled" || return 1
        if systemctl is-active --quiet citation-gemini.service; then
            systemctl stop citation-gemini.service || return 1
        fi
        # Refuse moving an application still running outside our service unit.
        for process in /proc/[0-9]*; do
            exe=$(readlink "$process/exe" 2>/dev/null || true)
            [ "$exe" != "$app/bin/citationvoice" ] || return 1
        done
    fi
    phase rolling-back || return 1
    if [ -e "$hook" ]; then
        mv -T -n "$hook" "$tx/preserved-hook" || return 1
        [ ! -e "$hook" ] || return 1
    fi
    if [ "$sandbox" = 1 ] && [ "${CITATION_TEST_CRASH_DURING_RECOVERY:-}" = hook ]; then kill -KILL $$; fi
    if [ -e "$app" ]; then
        mv -T -n "$app" "$tx/preserved-app" || return 1
        [ ! -e "$app" ] || return 1
    fi
    # Key, configuration and any newly-created account state are retained,
    # not deleted. Native bind mounts disappear only on the next reboot.
    phase rolled-back || return 1
    return 0
}
on_exit() {
    code=$?
    trap - EXIT
    if [ "$code" != 0 ] && [ "$operation" = apply ] && [ -f "$tx/phase" ]; then
        if ! recover; then echo 'Automatic rollback incomplete; run explicit recovery before reboot.' >&2; fi
    fi
    if [ -n "$lockdir" ]; then rm -f "$lockdir/pid"; rmdir "$lockdir" 2>/dev/null || true; fi
    exit "$code"
}
trap on_exit EXIT

if [ "$operation" = recover ]; then
    recover || die
    echo 'rolled-back; private files preserved; owner reboot required for stock runtime'
    exit 0
fi

# Fresh install only. Existing/legacy application or boot hooks are user work.
[ ! -e "$app" ] && [ ! -e "$hook" ] && [ ! -e "$tx" ] || die
[ -f "$incoming" ] && [ ! -L "$incoming" ] && [ "$(hash "$incoming")" = "$expected" ] || die
if [ "$sandbox" = 0 ]; then
    [ "$(hash /chrome/bin/iot_audio_capture)" = 92be267ce3dfa132a847713a87fa1f00a463af11447e33c333e1130a5fc0c9fc ] || die
    [ "$(hash /chrome/lib/libassistant.so)" = 045ee738323a16857ae24f59bd601377fb6963968075c41d25966461dd67de3b ] || die
    [ "$(hash /etc/asound-product.conf)" = db2a42816f7bd56b07ac868065dfa7bfdb490a4ed9c3a8525b5bc6d9df2e2884 ] || die
    grep -q '/data/alsa_looper.sh' /bin/alsa_looper.sh || die
    free=$(df -Pk "$data" | tail -n 1 | awk '{print $4}')
    case "$free" in ''|*[!0-9]*) die ;; esac
    payload_bytes=$(wc -c <"$incoming" | tr -d ' ')
    required=$(( (payload_bytes + 1023) / 1024 + 16384 ))
    [ "$required" -ge 65536 ] || required=65536
    [ "$free" -ge "$required" ] || die
    command -v systemd-run >/dev/null 2>&1 || die
    mv --help 2>&1 | grep -q -- '-T' || die
    mv --help 2>&1 | grep -q -- '-n' || die
fi

mkdir "$tx"
chmod 700 "$tx"
stage="$tx/stage"
mkdir "$stage"
tar -tf "$incoming" >"$tx/member-names"
tar -tvf "$incoming" >"$tx/member-types"
if grep -qv '^-' "$tx/member-types"; then die; fi
while IFS= read -r name; do
    case "$name" in
        bin/citationvoice|bin/iot_audio_capture48k|bin/libassistant.hotword-observe-v3.so|bin/supervise.sh|bin/ensure_native_capture.sh|bin/ensure_early_hotword.sh|boot/alsa_looper.sh|config.json|private/gemini.key|.release-owner|led/listening.bin|led/thinking.bin|led/speaking.bin|SHA256SUMS) ;;
        *) die ;;
    esac
done <"$tx/member-names"
[ "$(wc -l <"$tx/member-names" | tr -d ' ')" = 14 ] || die
[ "$(sort -u "$tx/member-names" | wc -l | tr -d ' ')" = 14 ] || die
tar -xf "$incoming" -C "$stage"
(cd "$stage" && sha256sum -c SHA256SUMS >/dev/null) || die
[ "$(cat "$stage/.release-owner")" = citation-source-installer-v1 ] || die
if [ "$sandbox" = 0 ]; then
    [ "$(hash "$stage/bin/iot_audio_capture48k")" = 984a7eb7d5cc8d8ff6b12ef71e4cf7385c1ee399fe49e520cb0ad3d63b80ef9d ] || die
    [ "$(hash "$stage/bin/libassistant.hotword-observe-v3.so")" = dbb1474a3f7e75a02b56f13c37d4899b5443605860168991734ccabc0dcf519d ] || die
fi
chmod 711 "$stage"
chmod 700 "$stage/private"
chmod 600 "$stage/config.json" "$stage/private/gemini.key"
chmod 755 "$stage/bin/"* "$stage/boot/alsa_looper.sh"
chmod 755 "$stage/bin" "$stage/boot" "$stage/led"
chmod 644 "$stage/led/"*.bin
mkdir "$stage/state"
chmod 700 "$stage/state"
if [ "$sandbox" = 0 ]; then
    # citationvoice reads config before dropping to the stock chrome UID/GID;
    # runtime key reads and state writes must still work after that drop.
    chown 1000:3003 "$stage/private" "$stage/private/gemini.key" "$stage/state"
fi
printf '%s\n' "$id" >"$stage/.transaction-id"
hash "$stage/boot/alsa_looper.sh" >"$tx/hook-sha256"
printf '%s\n' "$expected" >"$tx/payload-sha256"
echo absent >"$tx/original-app"
echo absent >"$tx/original-hook"
phase prepared
if [ "$sandbox" = 1 ] && [ "${CITATION_TEST_FAIL_AFTER:-}" = prepared ]; then exit 1; fi
if [ "$sandbox" = 1 ] && [ "${CITATION_TEST_CRASH_AFTER:-}" = prepared ]; then kill -KILL $$; fi
phase promoting
if [ "$sandbox" = 1 ] && [ "${CITATION_TEST_RACE_APP:-}" = 1 ]; then
    mkdir "$app"
    echo 'concurrent user work' >"$app/keep.txt"
fi
mv -T -n "$stage" "$app"
[ ! -e "$stage" ] && owned_app || die
sync
if [ "$sandbox" = 1 ] && [ "${CITATION_TEST_FAIL_AFTER:-}" = app ]; then exit 1; fi
if [ "$sandbox" = 1 ] && [ "${CITATION_TEST_CRASH_AFTER:-}" = app ]; then kill -KILL $$; fi

# Create the hook with no-clobber semantics, even if another actor races us.
temporary_hook="$data/.citation-hook-$id"
(set -C; cat "$app/boot/alsa_looper.sh" >"$temporary_hook")
chmod 755 "$temporary_hook"
ln "$temporary_hook" "$hook" || die
rm "$temporary_hook"
sync
if [ "$sandbox" = 1 ] && [ "${CITATION_TEST_FAIL_AFTER:-}" = hook ]; then exit 1; fi
if [ "$sandbox" = 1 ] && [ "${CITATION_TEST_CRASH_AFTER:-}" = hook ]; then kill -KILL $$; fi
phase installed-pending-reboot
if [ "$sandbox" = 1 ] && [ "${CITATION_TEST_CRASH_AFTER:-}" = committed ]; then kill -KILL $$; fi
echo 'installed-pending-reboot; no service started and no device rebooted'
