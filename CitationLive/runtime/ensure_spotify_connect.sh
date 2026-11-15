#!/bin/sh
set -eu

app=/data/opt/citation-gemini
config="$app/spotify-connect"
runtime=/run/citation-connect

fifo_ready() {
    [ -p "$runtime/audio" ] || return 1
    pcm_pid=$(systemctl show -p MainPID --value citation-spotify-pcm 2>/dev/null || systemctl show -p MainPID citation-spotify-pcm 2>/dev/null | sed 's/^MainPID=//')
    [ -n "$pcm_pid" ] && [ "$pcm_pid" != "0" ] && [ -d "/proc/$pcm_pid" ]
}

if [ "${1:-}" = "--sync-name" ]; then
    previous=
    previous_pid=
    while :; do
        if ! [ -p "$runtime/audio" ] || ! systemctl is-active --quiet citation-spotify-pcm || ! systemctl is-active --quiet citation-spotify-connect || \
           { [ -s "$app/bin/spotify_home_receiver.html" ] && ! systemctl is-active --quiet citation-spotify-home; }; then
            /bin/sh "$app/bin/ensure_spotify_connect.sh" >/dev/null 2>&1 || true
        fi
        pid=$(systemctl show -p MainPID citation-spotify-connect 2>/dev/null || true)
        name=$(curl -fsS --max-time 2 'http://127.0.0.1:8008/setup/eureka_info?params=name' 2>/dev/null || true)
        if [ -n "$name" ] && { [ "$name" != "$previous" ] || [ "$pid" != "$previous_pid" ]; }; then
            if curl -fsS --max-time 2 -H 'Content-Type: application/json' --data-binary "$name" \
                http://127.0.0.1:18767/set_device_name >/dev/null 2>&1; then
                previous=$name
                previous_pid=$pid
            fi
        fi
        sleep 10
    done
fi

if [ -s "$app/bin/spotify_home_receiver.html" ] &&
   [ -s "$app/bin/cast_receiver_framework.js" ] &&
   ! systemctl is-active --quiet citation-spotify-home; then
    systemctl reset-failed citation-spotify-home 2>/dev/null || true
    if systemctl cat citation-spotify-home >/dev/null 2>&1; then
        systemctl restart citation-spotify-home || true
    else
        # Firmware systemd-run does not support RestartSec/RestartUSec assignments.
        # Delay in the command itself to retain backoff on this older systemd.
        systemd-run --no-block --unit=citation-spotify-home --property=Restart=always \
            /bin/sh -c 'sleep 2; exec "$0" spotify-home' "$app/bin/citationvoice"
    fi
fi

[ -s "$config/client.tar.gz" ] && [ -s "$config/config.yml" ] || exit 0

if grep -q '/tmp/citation-connect/audio' "$config/config.yml" 2>/dev/null; then
    sed -i 's|/tmp/citation-connect/audio|/run/citation-connect/audio|g' "$config/config.yml"
fi

umask 077
mkdir -p "$runtime"
if [ ! -p "$runtime/audio" ]; then
    rm -f "$runtime/audio"
    mkfifo "$runtime/audio"
    systemctl stop citation-spotify-pcm 2>/dev/null || true
    systemctl reset-failed citation-spotify-pcm 2>/dev/null || true
fi
if [ ! -s "$runtime/go-librespot" ] || [ ! -x "$runtime/go-librespot" ]; then
    staging=$(mktemp -d "$runtime/unpack.XXXXXX")
    trap 'rm -f "$staging/go-librespot"; rmdir "$staging"' EXIT
    tar -xzf "$config/client.tar.gz" -C "$staging" go-librespot
    [ -s "$staging/go-librespot" ]
    chmod 700 "$staging/go-librespot"
    mv "$staging/go-librespot" "$runtime/go-librespot"
fi

if ! systemctl is-active --quiet citation-spotify-pcm; then
    systemctl reset-failed citation-spotify-pcm 2>/dev/null || true
    if systemctl cat citation-spotify-pcm >/dev/null 2>&1; then
        systemctl restart citation-spotify-pcm || true
    else
        systemd-run --no-block --unit=citation-spotify-pcm --property=Restart=on-failure \
            "$app/bin/citationvoice" spotify-pcm "$runtime/audio"
    fi
fi
if ! systemctl is-active --quiet citation-spotify-connect; then
    systemctl reset-failed citation-spotify-connect 2>/dev/null || true
    if systemctl cat citation-spotify-connect >/dev/null 2>&1; then
        systemctl restart citation-spotify-connect || true
    else
        systemd-run --no-block --unit=citation-spotify-connect --property=Restart=on-failure \
            /usr/bin/env HOME="$config" XDG_CACHE_HOME="$runtime" \
            "$runtime/go-librespot" --config_dir "$config" --conf device_type=speaker
    fi
fi
if ! systemctl is-active --quiet citation-spotify-name; then
    systemctl reset-failed citation-spotify-name 2>/dev/null || true
    if systemctl cat citation-spotify-name >/dev/null 2>&1; then
        systemctl restart citation-spotify-name || true
    else
        systemd-run --no-block --unit=citation-spotify-name --property=Restart=on-failure \
            /bin/sh "$app/bin/ensure_spotify_connect.sh" --sync-name
    fi
fi
