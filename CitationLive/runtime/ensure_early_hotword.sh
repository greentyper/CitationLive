#!/bin/sh
# Install the matching firmware's early hotword hook as a reversible bind mount.
set -eu
app=/data/opt/citation-gemini
target=/chrome/lib/libassistant.so
candidate=$app/bin/libassistant.hotword-observe-v3.so
original=045ee738323a16857ae24f59bd601377fb6963968075c41d25966461dd67de3b
patched=dbb1474a3f7e75a02b56f13c37d4899b5443605860168991734ccabc0dcf519d
hash() { sha256sum "$1" | cut -d ' ' -f 1; }

[ -r "$candidate" ] || exit 0
[ "$(hash "$candidate")" = "$patched" ] || exit 1
current=$(hash "$target")
[ "$current" != "$patched" ] || exit 0
[ "$current" = "$original" ] || exit 1

mount -o bind "$candidate" "$target"
# Restart only the existing assistant utility if it loaded the original file.
for proc in /proc/[0-9]*; do
    [ -r "$proc/cmdline" ] && [ -r "$proc/maps" ] || continue
    if tr '\000' ' ' <"$proc/cmdline" 2>/dev/null | grep -q '^/chrome/cast_shell .*--utility-sub-type=cast_assistant ' &&
       grep -q ' /chrome/lib/libassistant.so$' "$proc/maps" 2>/dev/null; then
        if ! kill -TERM "${proc##*/}" 2>/dev/null && [ -d "$proc" ]; then
            busybox umount -l "$target"
            exit 1
        fi
    fi
done
