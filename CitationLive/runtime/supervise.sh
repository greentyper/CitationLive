#!/bin/sh
# /data/opt/citation-gemini/bin/supervise.sh
# Manages citationvoice daemon lifecycle, auto-recovery, and environment initialization

app="/data/opt/citation-gemini"
state_dir="$app/state"
bin_dir="$app/bin"
home_dir="/data/home/chrome"
daemon_pid_file=/run/citationlive-daemon.pid

# Ensure runtime state directory exists before redirecting logs
mkdir -p "$state_dir"
chmod 700 "$state_dir"
chown -R 1000:3003 "$state_dir" 2>/dev/null || true
chmod -R u+rwX "$state_dir" 2>/dev/null || true

# Redirect all supervisor logs to service.log so calling shell can detach cleanly
exec >> "$state_dir/service.log" 2>&1

# Ignore SIGHUP so closing tty/session does not terminate the supervisor
trap '' HUP

mkdir -p "$home_dir"
chmod 755 "$home_dir"
chown 1000:3003 "$home_dir" 2>/dev/null || true

# Export standard environment variables required by Go runtime and native subsystems
export HOME="/data/home/chrome"
export TMPDIR="/tmp"
export PATH="$bin_dir:$app:/bin:/usr/bin:/sbin:/xbin"

# Determine daemon executable
daemon=""
if [ -x "$bin_dir/citationvoice" ]; then
    daemon="$bin_dir/citationvoice"
elif [ -x "$app/citationvoice" ]; then
    daemon="$app/citationvoice"
elif [ -x "$app/citation-gemini" ]; then
    daemon="$app/citation-gemini"
fi

if [ -z "$daemon" ]; then
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] ERROR: citationvoice executable not found in $bin_dir or $app" >&2
    exit 1
fi

# Single instance enforcement via file locking
lock_file="$state_dir/supervisor.lock"
pid_file="$state_dir/supervisor.pid"

if [ -x /usr/bin/flock ]; then
    exec 200>"$lock_file"
    if ! /usr/bin/flock -n 200; then
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] Supervisor instance already running, exiting"
        exit 0
    fi
fi

echo $$ > "$pid_file"

child=
capture_monitor=
monitor_native_capture() {
    monitor_daemon=$1
    monitor_sleep=
    trap 'if [ -n "$monitor_sleep" ]; then kill "$monitor_sleep" 2>/dev/null || true; fi; exit 0' TERM INT
    while kill -0 "$monitor_daemon" 2>/dev/null; do
        if grep -q '"microphone_backend"[[:space:]]*:[[:space:]]*"native_capture_service"' "$app/config.json"; then
            /bin/sh "$bin_dir/ensure_native_capture.sh" || echo "Native capture restart failed; retrying"
            if [ -f /tmp/citation-native-capture.log ]; then
                capture_log_size=$(wc -c </tmp/citation-native-capture.log)
                if [ "$capture_log_size" -gt 262144 ]; then
                    : >/tmp/citation-native-capture.log
                fi
            fi
        fi
        sleep 5 &
        monitor_sleep=$!
        wait "$monitor_sleep" 2>/dev/null || true
        monitor_sleep=
    done
}
stop_native() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] Supervisor received shutdown signal, terminating daemon..."
    if [ -n "$capture_monitor" ]; then
        kill "$capture_monitor" 2>/dev/null || true
        wait "$capture_monitor" 2>/dev/null || true
    fi
    if [ -n "$child" ]; then
        kill "$child" 2>/dev/null || true
        wait "$child" 2>/dev/null || true
    fi
    rm -f "$pid_file" "$daemon_pid_file" 2>/dev/null || true
    exit 0
}
trap stop_native TERM INT

# Wait for network readiness (default route established)
net_wait=0
while ! ip route 2>/dev/null | grep -q default; do
    sleep 1
    net_wait=$((net_wait + 1))
    if [ $net_wait -ge 30 ]; then
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] Network wait timeout (30s reached), proceeding..."
        break
    fi
done

# Wait for WAMP message router readiness (port 9998 open)
wamp_wait=0
while ! netstat -tl 2>/dev/null | grep -q :9998; do
    sleep 1
    wamp_wait=$((wamp_wait + 1))
    if [ $wamp_wait -ge 30 ]; then
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] WAMP wait timeout (30s reached), proceeding..."
        break
    fi
done

echo "[$(date '+%Y-%m-%d %H:%M:%S')] Supervisor initialized (PID $$), starting daemon loop: $daemon"

if [ -f "$bin_dir/auto_update.sh" ]; then
    /bin/sh "$bin_dir/auto_update.sh" boot 200>&- || echo "CitationLive auto-update could not start"
fi

if [ -f "$bin_dir/ensure_spotify_connect.sh" ]; then
    /bin/sh "$bin_dir/ensure_spotify_connect.sh" 200>&- || echo "Native Spotify Connect unavailable"
fi

if [ -f "$bin_dir/ensure_dj_wake.sh" ]; then
    /bin/sh "$bin_dir/ensure_dj_wake.sh" 200>&- || echo "Custom DJ wake word unavailable"
fi

if [ -f "$bin_dir/ensure_cast_equalizer.sh" ]; then
    /bin/sh "$bin_dir/ensure_cast_equalizer.sh" 200>&- || echo "Cast equalizer unavailable; using stock audio"
fi

if [ -f "$bin_dir/ensure_cast_spotify_filter.sh" ]; then
    /bin/sh "$bin_dir/ensure_cast_spotify_filter.sh" 200>&- || echo "Spotify Cast filter unavailable"
fi

if grep -q '"replace_google"[[:space:]]*:[[:space:]]*true' "$app/config.json" &&
   grep -q '"auto_wake"[[:space:]]*:[[:space:]]*true' "$app/config.json" &&
   [ -f "$bin_dir/ensure_early_hotword.sh" ]; then
    /bin/sh "$bin_dir/ensure_early_hotword.sh" || echo "Early hotword hook unavailable; using stock wake routing"
fi

if [ ! -f /tmp/citation_boot_sparkle_played ] && [ -f "$app/cues/boot_sparkle.wav" ]; then
    touch /tmp/citation_boot_sparkle_played
    (sleep 1 && "$daemon" mixer-play "$app/cues/boot_sparkle.wav" 200>&- || true) &
fi

# Continuous supervisor loop with 2-second backoff on unexpected exit
while [ ! -f "$app/disabled" ]; do
    if grep -q '"microphone_backend"[[:space:]]*:[[:space:]]*"native_capture_service"' "$app/config.json"; then
        if ! /bin/sh "$bin_dir/ensure_native_capture.sh"; then
            echo "Native capture not ready; retrying in 2 seconds"
            sleep 2
            continue
        fi
    fi
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] Starting $daemon serve..."
    "$daemon" serve "$app/config.json" 200>&- &
    child=$!
    printf '%s\n' "$child" > "$daemon_pid_file"
    monitor_native_capture "$child" 200>&- &
    capture_monitor=$!
    wait "$child" || true
    rm -f "$daemon_pid_file"
    kill "$capture_monitor" 2>/dev/null || true
    wait "$capture_monitor" 2>/dev/null || true
    capture_monitor=
    child=

    if [ -f "$app/disabled" ]; then
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] Service explicitly disabled via marker $app/disabled, exiting"
        break
    fi

    echo "[$(date '+%Y-%m-%d %H:%M:%S')] Daemon exited unexpectedly, restarting in 2 seconds..."
    sleep 2
done

rm -f "$pid_file" 2>/dev/null || true
