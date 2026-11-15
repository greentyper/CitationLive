#!/bin/sh
# /data/alsa_looper.sh - Persistent autostart hook on writable /data UBIFS
# Called on boot by /lib/systemd/system/system-manager.service via /bin/alsa_looper.sh init

if [ "$1" = "X" ]; then
    shift
fi

dowhat="$1"

if [ "$dowhat" = "init" ]; then
    /bin/sh /bin/alsa_looper.sh X "$@" || exit $?
    supervisor=/data/opt/citation-gemini/bin/supervise.sh
    if [ -x "$supervisor" ]; then
        # ExecStartPre cannot keep its own background processes alive.
        /usr/bin/systemd-run --no-block --unit=citation-gemini /bin/sh "$supervisor" || true
    fi
    exit 0
fi

# Always delegate back to /bin/alsa_looper.sh with 'X' flag to prevent infinite recursion
# and allow native ALSA loopers and audio devices to initialize normally.
exec /bin/sh /bin/alsa_looper.sh X "$@"
