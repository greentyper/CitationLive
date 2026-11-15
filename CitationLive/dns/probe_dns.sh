#!/bin/sh
# Probe DNS + the exact Gemini endpoints used by citationvoice.
# $1 = set|system, $2 = space-separated DNS servers
MODE="$1"
SERVERS="$2"
BAK=/data/local/tmp/citation_resolv.bak
WD=/data/opt/citation-gemini/state/dns-watchdog.pid
ENSURE=/data/opt/citation-gemini/bin/ensure_dns.sh
cp /run/resolv.conf "$BAK" 2>/dev/null
stop_watchdog() {
    [ -f "$WD" ] || return 0
    pid=$(cat "$WD" 2>/dev/null)
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
    rm -f "$WD"
}
restart_watchdog() {
    [ -x "$ENSURE" ] && /bin/sh "$ENSURE" >/dev/null 2>&1 || true
}
restore() {
    cp "$BAK" /run/resolv.conf 2>/dev/null
    restart_watchdog
}
trap restore EXIT INT TERM
stop_watchdog

if [ "$MODE" = "set" ]; then
    {
        echo "# CitationLive DNS probe"
        for s in $SERVERS; do echo "nameserver $s"; done
        echo "options timeout:2 attempts:2"
    } > /run/resolv.conf
fi

echo "RESOLV:"
cat /run/resolv.conf
echo "DNS:"
timeout 6 nslookup generativelanguage.googleapis.com 2>&1 | tail -n 8

KEY=$(cat /data/opt/citation-gemini/gemini.key 2>/dev/null)
echo "REST:"
curl --max-time 15 -sS -o /dev/null -w '%{http_code} %{remote_ip}\n' \
    "https://generativelanguage.googleapis.com/v1beta/models?key=$KEY" 2>&1

echo "WS:"
curl --max-time 8 -sS -o /dev/null -w '%{http_code} %{remote_ip}\n' \
    -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
    -H 'Sec-WebSocket-Version: 13' \
    -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
    -H "X-Goog-Api-Key: $KEY" \
    "https://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent" 2>&1

restore
trap - EXIT
