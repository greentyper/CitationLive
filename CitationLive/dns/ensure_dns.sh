#!/bin/sh
# CitationLive DNS runtime hook.
# Reads /data/opt/citation-gemini/dns.conf and applies the chosen resolver.
# Rootfs is read-only; /run/resolv.conf is tmpfs, so this is the only safe place.
APP=/data/opt/citation-gemini
CONF="$APP/dns.conf"
STATE="$APP/state"
ORIG="$STATE/resolv.conf.orig"
PIDFILE="$STATE/dns-watchdog.pid"
LOG="$STATE/dns.log"
RUN=/run/resolv.conf

log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" >> "$LOG" 2>/dev/null
}

read_conf() {
    MODE=$(sed -n 's/^MODE=//p' "$CONF" 2>/dev/null | head -n1 | tr -d '\r')
    SERVERS=$(sed -n 's/^SERVERS=//p' "$CONF" 2>/dev/null | head -n1 | tr -d '\r')
}

valid_servers() {
    [ -n "$SERVERS" ] || return 1
    for s in $SERVERS; do
        case "$s" in
            *[!0-9a-fA-F:.]*|'') return 1 ;;
        esac
    done
    return 0
}

save_orig() {
    [ -f "$ORIG" ] || cp "$RUN" "$ORIG" 2>/dev/null || true
}

restore_orig() {
    [ -f "$ORIG" ] || return 0
    if ! cmp -s "$ORIG" "$RUN" 2>/dev/null; then
        cp "$ORIG" "$RUN" 2>/dev/null || true
        log "restored original resolv.conf"
    fi
}

apply_dns() {
    save_orig
    tmp="/tmp/resolv.conf.citation.$$"
    {
        echo "# CitationLive DNS ($MODE)"
        for s in $SERVERS; do echo "nameserver $s"; done
        echo "options timeout:2 attempts:2"
    } > "$tmp"
    if ! cmp -s "$tmp" "$RUN" 2>/dev/null; then
        cp "$tmp" "$RUN" 2>/dev/null || { rm -f "$tmp"; log "cannot write $RUN"; return 1; }
        log "applied DNS: $SERVERS"
    fi
    rm -f "$tmp"
    if command -v nslookup >/dev/null 2>&1; then
        if ! timeout 6 nslookup generativelanguage.googleapis.com >/dev/null 2>&1; then
            log "DNS test failed for $SERVERS; restoring"
            restore_orig
            return 1
        fi
    fi
    return 0
}

stop_watchdog() {
    [ -f "$PIDFILE" ] || return 0
    pid=$(cat "$PIDFILE" 2>/dev/null)
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
    rm -f "$PIDFILE"
}

start_watchdog() {
    if [ -f "$PIDFILE" ]; then
        pid=$(cat "$PIDFILE" 2>/dev/null)
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            return 0
        fi
    fi
    (
        while :; do
            sleep 30
            m=$(sed -n 's/^MODE=//p' "$CONF" 2>/dev/null | head -n1 | tr -d '\r')
            case "$m" in
                public|custom) read_conf; valid_servers && apply_dns >/dev/null 2>&1 ;;
                *) exit 0 ;;
            esac
        done
    ) &
    echo $! > "$PIDFILE"
}

[ -f "$CONF" ] || exit 0
read_conf

case "$MODE" in
    off|system)
        stop_watchdog
        restore_orig
        ;;
    public|custom)
        if ! valid_servers; then
            log "invalid SERVERS: $SERVERS"
            exit 1
        fi
        apply_dns || exit 1
        start_watchdog
        ;;
    *)
        log "unknown MODE: $MODE"
        exit 1
        ;;
esac
exit 0
