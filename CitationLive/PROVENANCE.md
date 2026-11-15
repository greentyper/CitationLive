# Bundled device components / происхождение бинарников

The MIT license in `LICENSE` covers the CitationLive source code and the
`citationvoice` binary built from `src/`. It does **not** grant rights to the
following vendor components that are included only so the installer can work
with a speaker you own:

| File | Origin | Status |
|---|---|---|
| `dist/iot_audio_capture48k` | Capture helper extracted from the Citation One firmware and prepared for the 48 kHz pipeline. | Vendor component; rights remain with its owners. Do not redistribute separately. |
| `dist/libassistant.hotword-observe-v3.so` | Patched vendor `libassistant` library from the speaker firmware, used by the early-hotword observer. | Vendor component; rights remain with its owners. Do not redistribute separately. |
| `runtime/cast_receiver_framework.js` | Google Cast Receiver Framework (CAF), loaded by the local Cast receiver page. | Proprietary Google component; rights remain with Google. Do not redistribute separately. |

These files are provided for interoperability with the device only. If you do
not own the device or do not accept the vendor terms, remove them from your
copy and do not run the corresponding features.

The `go-librespot` runtime in `spotify-connect/client.tar.gz` is GPL-3.0; see
`spotify-connect/LICENSE-GPL-3.0.txt` and `spotify-connect/SOURCE_OFFER.md`.
