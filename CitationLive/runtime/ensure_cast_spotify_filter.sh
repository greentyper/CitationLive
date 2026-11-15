#!/bin/sh
set -eu

apps=/data/opt/citation-gemini/bin/cast-apps-without-spotify.json
process_config=/chrome/process.json
overlay=/tmp/citation-cast-process.json
original_flag=--app-config-path=/fakeapp.conf
filter_flag=--app-config-path=$apps

[ -r "$apps" ] || exit 0

# The autostart hook can run before the stock Cast process manager is ready.
# Do not stop/start Cast during that boot window.
attempt=0
while [ ! -s /tmp/process_manager.pid ] || ! pidof cast_shell >/dev/null 2>&1; do
    attempt=$((attempt + 1))
    [ "$attempt" -le 60 ] || exit 1
    sleep 1
done

filter_running() {
    for pid in $(pidof cast_shell 2>/dev/null || true); do
        if tr '\000' '\n' <"/proc/$pid/cmdline" 2>/dev/null | grep -qxF -- "$filter_flag"; then
            return 0
        fi
    done
    return 1
}

if [ ! "$overlay" -ef "$process_config" ]; then
    [ "$(grep -Fc -- "$original_flag" "$process_config")" -eq 1 ] || exit 1
    if awk -v path="$process_config" '$2 == path { found = 1 } END { exit !found }' /proc/mounts; then
        exit 1
    fi
    sed "s|$original_flag|$filter_flag|" "$process_config" >"$overlay"
    chmod 644 "$overlay"
    mount --bind "$overlay" "$process_config"
fi

if filter_running; then
    exit 0
fi
if ! /chrome/cast_cli stop cast && pidof cast_shell >/dev/null 2>&1; then
    umount "$process_config"
    exit 1
fi
if start cast_receiver; then
    for attempt in 1 2 3 4 5 6 7 8 9 10 11 12; do
        if filter_running && /chrome/cast_cli status >/dev/null 2>&1; then
            exit 0
        fi
        sleep 1
    done
fi

umount "$process_config"
/chrome/cast_cli stop cast || true
start cast_receiver
exit 1
