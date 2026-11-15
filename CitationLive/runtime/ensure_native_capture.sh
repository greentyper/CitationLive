#!/bin/sh
# Native capture observer engine. Only the stock capture service is restarted.
# A detached native process must never retain the supervisor's flock.
exec 200>&-
# The supervisor loop and its monitor can call this script at the same time.
# Serialize them so two capture processes never compete for the microphone.
exec 201>"/tmp/citation-native-capture.lock"
flock -n 201 2>/dev/null || exit 0
app=/data/opt/citation-gemini
wanted="--alsa-input-device=mics_gva"
delay="--voice-call-aec-delay-params=96:48"
capture_binary="$app/bin/iot_audio_capture48k"
capture_log=/tmp/citation-native-capture.log
# A live process is not enough: after a failed bind it keeps reading ALSA but
# no longer serves the abstract observer socket. Gemini then starts with a
# silent microphone. Treat a missing socket as an unhealthy capture service.
service_socket_present() {
    grep -q '@/tmp/capture-service' /proc/net/unix 2>/dev/null
}
# Preserve the firmware's gain profile. Capture observers may precede its
# post-amplifier, so exchanging pre/post gain does not preserve capture level.
gain="--voice-call-aec-gain-fixed=31:5:36:0:36:0:36:0:36:0:36:0:36:0:36:0:24:12:24:12:14:22"
healthy=""
stale=""
for capture_pid in $(pidof iot_audio_capture iot_audio_capture48k 2>/dev/null); do
    capture_exe=$(readlink /proc/$capture_pid/exe 2>/dev/null)
    case "$capture_exe" in /chrome/bin/iot_audio_capture|"$capture_binary") ;; *) continue ;; esac
    # Run as uid 1000 (chrome) so ALSA dsnoop SysV sem/shm (key 8421..8427, perms 0600)
    # remain accessible to cast_assistant/cast_shell (also uid 1000) for hotword detection.
    if [ "$capture_exe" = "$capture_binary" ] &&
       grep -qE '^Uid:[[:space:]]*1000([[:space:]]|$)' /proc/$capture_pid/status 2>/dev/null &&
       tr '\000' '\n' </proc/$capture_pid/cmdline 2>/dev/null | grep -Fxq -- "$wanted" &&
       tr '\000' '\n' </proc/$capture_pid/cmdline 2>/dev/null | grep -Fxq -- "$delay" &&
       tr '\000' '\n' </proc/$capture_pid/cmdline 2>/dev/null | grep -Fxq -- "$gain" &&
       tr '\000' '\n' </proc/$capture_pid/environ 2>/dev/null | grep -Fxq "ALSA_CONFIG=/etc/asound-product.conf"; then
        if [ -z "$healthy" ]; then
            healthy="$capture_pid"
        else
            # More than one observer competes for the mic. Keep exactly one.
            stale="$stale $capture_pid"
        fi
    else
        stale="$stale $capture_pid"
    fi
done
if [ -n "$healthy" ] && ! service_socket_present; then
    # The process survived but its observer listener did not. Restart it so a
    # new Gemini session does not open a silent microphone.
    stale="$stale $healthy"
    healthy=""
fi
if [ -n "$healthy" ]; then
    for capture_pid in $stale; do
        kill -TERM "$capture_pid" 2>/dev/null || true
    done
    echo "$healthy" >"$app/state/native-capture-service.pid"
    exit 0
fi
for capture_pid in $stale; do
    kill -TERM "$capture_pid" 2>/dev/null || true
done
[ -z "$stale" ] || sleep 1
# The abstract socket can survive briefly after SIGTERM. Wait for the old
# listener to disappear before binding a new one, otherwise the new process
# may fail to bind while the post-start check still sees the stale socket.
for _ in 1 2 3 4 5 6; do
    service_socket_present || break
    sleep 0.5
done
if service_socket_present; then
    for capture_pid in $stale; do
        kill -KILL "$capture_pid" 2>/dev/null || true
    done
    sleep 0.5
fi
"$app/bin/citationvoice" clean-ipc 2>/dev/null || true
umask 027
: >"$capture_log"
chown chrome:system "$capture_log" 2>/dev/null || chown 1000:100 "$capture_log" 2>/dev/null || true
chmod 640 "$capture_log" 2>/dev/null || true
su -s /bin/sh chrome -c "umask 027; ALSA_CONFIG=/etc/asound-product.conf nohup '$capture_binary' '$delay' '$gain' '$wanted' >>'$capture_log' 2>&1 </dev/null &"
# The process can read ALSA before its observer socket is ready. Wait for the
# listener itself, not just for a PID, before letting the daemon start.
for _ in 1 2 3 4 5 6; do
    service_socket_present && break
    sleep 0.5
done
service_socket_present || { echo "Native capture service did not open its observer socket" >&2; exit 1; }
capture_pid=""
for pid in $(pidof iot_audio_capture48k 2>/dev/null); do
    if grep -qE '^Uid:[[:space:]]*1000([[:space:]]|$)' /proc/$pid/status 2>/dev/null; then
        capture_pid="$pid"
        break
    fi
done
[ -n "$capture_pid" ] || { echo "Native capture service failed to start" >&2; exit 1; }
echo "$capture_pid" >"$app/state/native-capture-service.pid"
kill -0 "$capture_pid" 2>/dev/null || { echo "Native capture service failed to start" >&2; exit 1; }
