# go-librespot — source offer / предложение исходников

`client.tar.gz` in this directory contains a build of **go-librespot**
(version string `0.10.5-citation-connect-session`), licensed under **GPL-3.0**.
The full license text is in `LICENSE-GPL-3.0.txt`.

The build uses upstream commit `90a4fdaf0314269e3fa25698a5f4270c026d2c03`
with all CitationLive source changes in
[`patches/go-librespot-dj-segment-jump.patch`](patches/go-librespot-dj-segment-jump.patch).
Apply the patch to that commit with `git apply`.

The bundled binary was built with Go 1.27.3 for `linux/arm`, `GOARM=7`
and `CGO_ENABLED=1`. Zig 0.14.1 targeted `arm-linux-gnueabihf.2.26` with
the speaker's ALSA, FLAC, mpg123, Ogg and Vorbis libraries. The build sets
`CGO_CFLAGS=-DMPG123_ENUM_API` and builds `./cmd/daemon` with
`-trimpath -buildvcs=false -ldflags '-s -w -X github.com/devgianlu/go-librespot.version=0.10.5-citation-connect-session'`.
The final packaging step removes C debug sections with `zig objcopy --strip-debug`.

[Upstream source on GitHub](https://github.com/devgianlu/go-librespot/tree/90a4fdaf0314269e3fa25698a5f4270c026d2c03)

For the exact binary bundled in this release, the corresponding source is
available from the maintainer on request through this repository's issue
tracker. This written offer is valid for at least three years from the
publication date of the release that contains this file.

`client.tar.gz` is a runtime dependency of the local Spotify Connect bridge.
It is not covered by the MIT license of CitationLive.
