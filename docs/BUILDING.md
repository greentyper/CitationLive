# Reproduce the release binary

Use Go **1.27.3** (the version embedded in the shipped executable).
Run these commands from `src/` in a POSIX shell.

```sh
go mod verify
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 \
  go build -trimpath -buildvcs=false -ldflags='-s -w' -o /tmp/citationvoice .
cmp /tmp/citationvoice ../CitationLive/dist/citationvoice
```

Use `../MANIFEST.sha256` to check the distributed executable's checksum.

The daemon uses Linux syscalls. A native Windows `go test ./...` is not
supported. On Linux, run:

```sh
go test -race ./...
go vet ./...
```

To run the tests on the ARM speaker, cross-compile with:

```sh
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 \
  go test -c -o /tmp/citationvoice.test .
```

Copy that test executable to a temporary directory on the speaker, make it
executable and run it with `-test.v -test.timeout=120s`. Inspect the printed
PASS/FAIL and the **remote** exit status: the stock ADB protocol can return
host exit code zero even when a remote command fails. Delete only the test
executable afterwards. These tests use fake transports; they do not replace
real microphone, playback, OAuth or reboot checks.

From the release root (`docs/..`):

```sh
python -B scripts/preflight.py
python -B scripts/package_release.py
```

The package command writes the ZIP and SHA256SUMS.txt to the parent directory
and updates MANIFEST.sha256 in this repository. It does not rebuild Go.
