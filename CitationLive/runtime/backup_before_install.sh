#!/bin/sh
set -eu
umask 077

id="${1:-}"
case "$id" in
    ''|*[!0-9T_-]*) exit 2 ;;
esac

app=/data/opt/citation-gemini
hook=/data/alsa_looper.sh
backup_root=/data/opt/citation-gemini-backups
backup_dir="$backup_root/$id"
manifest="$backup_dir/manifest"

mkdir -p "$backup_dir"
chmod 700 "$backup_root" "$backup_dir"
: >"$manifest"

archive_if_exists() {
    base=$1
    relative=$2
    archive=$3
    if [ -e "$base/$relative" ] || [ -L "$base/$relative" ]; then
        tar -czf "$backup_dir/$archive" -C "$base" "$relative"
        test -s "$backup_dir/$archive"
        printf '%s\n' "$archive" >>"$manifest"
    fi
}

archive_if_exists /data/opt citation-gemini citation-gemini.tar.gz
archive_if_exists /data alsa_looper.sh alsa_looper.tar.gz
archive_if_exists /data/home chrome data-home-chrome.tar.gz
archive_if_exists / chrome/lib/libassistant.so chrome-libassistant.tar.gz
archive_if_exists / chrome/process.json chrome-process.tar.gz
archive_if_exists /tmp citation-native-capture.log tmp-native-capture.log.tar.gz

if [ ! -s "$manifest" ]; then
    printf '%s\n' "no-existing-files" >"$manifest"
fi

sync
printf '%s\n' "$backup_dir"
