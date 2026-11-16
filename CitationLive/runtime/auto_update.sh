#!/bin/sh
set -eu
umask 077

app=/data/opt/citation-gemini
bin=$app/bin
state=$app/.auto-update
pid_file=/run/citationlive-daemon.pid
unit=citationlive-autoupdate
repo=https://api.github.com/repos/greentyper/CitationLive
script=$bin/auto_update.sh

enabled() {
    test -f "$state/enabled"
}

schedule() {
    enabled || return 0
    systemctl is-active --quiet "$unit.timer" && return 0
    systemd-run --no-block --unit="$unit" --on-active=5min --on-unit-inactive=6h \
        /bin/sh "$script" check
}

# The worker and its journal are root-owned and shared with boot recovery.
lock_update() {
    test ! -L "$state"
    mkdir -p "$state"
    chmod 700 "$state"
    exec 9>"$state/lock"
    flock -n 9
}

daemon_pid() {
    pid=$(cat "$pid_file" 2>/dev/null) || return 1
    case "$pid" in ''|*[!0-9]*) return 1 ;; esac
    test "$(readlink "/proc/$pid/exe" 2>/dev/null)" = "$1" || return 1
    printf '%s\n' "$pid"
}

clear_rollback() {
    # Clear the transaction before dropping its backup. If interrupted, the
    # backup can be discarded on the next boot or check.
    rm -f "$state/pending" && rm -f "$state/previous"
}

clear_staging() {
    rm -f "$state/candidate" "$state/pending.new" "$state/previous.new" "$state/restore"
}

restore_previous() {
    read -r name < "$state/pending"
    case "$name" in citationvoice|citationvoice.real) ;; *) return 1 ;; esac
    live=$bin/$name
    failed_pid=$(daemon_pid "$live" || true)
    ln -f "$state/previous" "$state/restore" &&
        mv -f "$state/restore" "$live" || return 1
    if test -n "$failed_pid"; then
        kill -TERM "$failed_pid" 2>/dev/null || true
    fi
    clear_rollback || return 1
    printf '%s\n' 'Restored previous CitationLive executable.'
}

recover_update() {
    if test -f "$state/pending"; then
        restore_previous || return 1
    else
        rm -f "$state/previous" || return 1
    fi
    clear_staging
}

github_get() {
    curl --proto '=https' --fail --silent --show-error --connect-timeout 8 --max-time 45 "$@"
}

blob_sha() {
    bytes=$(wc -c < "$1" | tr -d '[:space:]')
    { printf 'blob %s\000' "$bytes"; cat "$1"; } | sha1sum | cut -d ' ' -f 1
}

assistant_idle() {
    status=$(curl -fsS --max-time 3 http://127.0.0.1:8766/status) || return 1
    printf '%s' "$status" | grep -Eq '"state"[[:space:]]*:[[:space:]]*"idle"'
}

speaker_idle() {
    assistant_idle || return 1
    if systemctl is-active --quiet citation-spotify-connect.service; then
        spotify=$(curl -fsS --max-time 3 http://127.0.0.1:18767/status) || return 1
        if printf '%s' "$spotify" | grep -Eq '"buffering"[[:space:]]*:[[:space:]]*true'; then
            return 1
        fi
        test -z "$spotify" || printf '%s' "$spotify" | grep -Eq '"(paused|stopped)"[[:space:]]*:[[:space:]]*true'
    fi
}

wait_ready() {
    elapsed=0
    while test "$elapsed" -lt 25; do
        new_pid=$(daemon_pid "$1" || true)
        if test -n "$new_pid" && test "$new_pid" != "$2"; then
            status=$(curl -fsS --max-time 2 http://127.0.0.1:8766/status) || status=
            if printf '%s' "$status" | grep -Eq '"native"[[:space:]]*:[[:space:]]*true'; then
                return 0
            fi
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done
    return 1
}

restart_spotify_helpers() {
    for helper in citation-spotify-pcm.service citation-spotify-home.service; do
        if systemctl is-active --quiet "$helper"; then
            systemctl restart "$helper" || return 1
        fi
    done
}

check() {
    enabled && test ! -e "$app/disabled" || return 0
    lock_update || return 0
    recover_update
    speaker_idle || return 0

    name=citationvoice
    if test "$(head -c 2 "$bin/citationvoice")" = '#!'; then
        name=citationvoice.real
    fi
    target=$bin/$name
    live_pid=$(daemon_pid "$target") || return 0
    meta=$(github_get --max-filesize 65536 "$repo/contents/CitationLive/dist/citationvoice?ref=main")
    sha=$(printf '%s\n' "$meta" | sed -n 's/^[[:space:]]*"sha":[[:space:]]*"\([0-9a-f]*\)".*/\1/p')
    size=$(printf '%s\n' "$meta" | sed -n 's/^[[:space:]]*"size":[[:space:]]*\([0-9]*\).*/\1/p')
    test "${#sha}" -eq 40 && test -n "$size" && test "$size" -gt 0 && test "$size" -le 33554432
    test "$sha" != "$(blob_sha "$target")" || return 0
    test "$sha" != "$(cat "$state/rejected" 2>/dev/null || true)" || return 0

    candidate=$state/candidate
    cleanup() {
        if test -f "$state/pending"; then
            if restore_previous; then
                printf '%s\n' "$sha" > "$state/rejected"
                restart_spotify_helpers || true
            else
                echo 'CitationLive rollback failed; recovery will retry.' >&2
            fi
        fi
        clear_staging
    }
    trap cleanup EXIT
    trap 'exit 1' HUP INT TERM

    github_get --max-filesize "$size" -H 'Accept: application/vnd.github.raw+json' \
        -o "$candidate" "$repo/git/blobs/$sha"
    test "$(wc -c < "$candidate" | tr -d '[:space:]')" = "$size"
    test "$(blob_sha "$candidate")" = "$sha"
    chmod 755 "$candidate"
    timeout 5 "$candidate" info | grep -Eq '"architecture"[[:space:]]*:[[:space:]]*"arm"'
    enabled && speaker_idle && test "$(daemon_pid "$target")" = "$live_pid" || return 0

    ln -f "$target" "$state/previous.new"
    mv -f "$state/previous.new" "$state/previous"
    printf '%s\n' "$name" > "$state/pending.new"
    mv -f "$state/pending.new" "$state/pending"
    mv -f "$candidate" "$target"
    kill -TERM "$live_pid"
    wait_ready "$target" "$live_pid"
    restart_spotify_helpers
    clear_rollback
    rm -f "$state/rejected"
    printf 'Updated CitationLive to GitHub blob %s\n' "$sha"
}

case "${1:-}" in
    enable)
        lock_update
        : > "$state/enabled"
        if ! schedule; then
            rm -f "$state/enabled"
            exit 1
        fi
        ;;
    disable)
        rm -f "$state/enabled"
        if systemctl is-active --quiet "$unit.timer"; then
            systemctl stop "$unit.timer"
        fi
        ;;
    status)
        if enabled; then echo enabled; else echo disabled; fi
        ;;
    boot)
        if test -d "$state" && lock_update; then
            recover_update
        fi
        schedule
        ;;
    check) check ;;
    *) echo 'Usage: auto_update.sh enable|disable|status|boot|check' >&2; exit 2 ;;
esac
