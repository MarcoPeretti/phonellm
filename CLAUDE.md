# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
make build              # -> bin/phonellm
make check              # go vet + go test -race ./...
make test               # go test -race ./...
make echo               # run in echo mode (see "Milestone 0" below)
make run                # run against the real Realtime API
make build-linux-arm64  # cross-compile from macOS for a Pi / ARM64 box
```

Run a single test: `go test ./internal/bridge/ -run TestBurstIsDrainedInOrderedFrames -v`

Config is environment-driven, and the binary reads `.env` from its working directory
itself — do not source it first. Precedence is deliberately **flag > `.env` > shell
environment**: the file beating the shell prevents a stale `set -a && . ./.env` export
from silently overriding an edited file, and the `-echo` flag exists so echo mode cannot
be overridden by whatever `.env` happens to say. `PHONELLM_ENV_FILE` points elsewhere.

## What this is

An LLM answering machine for a single Fritz!Box line. It registers with the Fritz!Box as
an ordinary LAN IP telephone, answers, and bridges call audio to the OpenAI Realtime API.
One static binary, nothing exposed to the internet.

Two decisions shape everything else, and both are easy to break accidentally:

**There is no PBX.** The Fritz!Box *is* the PBX — it registers up to ten SIP endpoints and
routes numbers to them. Do not reintroduce Asterisk/FreePBX; the whole point was removing
them.

**The audio path is a byte passthrough.** The Fritz!Box negotiates G.711 and the Realtime
API accepts `audio/pcma`/`audio/pcmu` directly, so RTP payload goes to the WebSocket
base64-encoded and unmodified. No transcoding, no resampling anywhere in this repo. If you
find yourself adding a resampler, something upstream has gone wrong — check codec
negotiation first.

## Architecture

Call flow, end to end:

`cmd/phonellm` loads config and resolves the LAN bind address (SIP puts the address in the
message body, so binding to the wildcard yields an unreachable Contact) → `internal/telephony`
registers and serves → on INVITE: `Trying` → `Ringing` → ring delay → `AnswerOptions` with
codecs pinned → reads negotiated `MediaProps` → maps the RTP payload type to a Realtime
audio format → `internal/realtime` dials and configures the session → `internal/bridge`
runs three goroutines until the call ends → `internal/notify` gets the transcript.

The bridge's three goroutines, all cancelled by a shared context:
1. **caller → model**: blocking reads off the RTP reader, naturally paced by the caller's clock.
2. **model → caller**: a 20 ms ticker writing exactly one frame per tick.
3. **event loop**: audio deltas, barge-in, transcripts, silence timeout.

### Load-bearing details

- **The output pacer** (`internal/bridge`) is where call quality lives. The model emits
  bursts; RTP demands one 160-byte frame every 20 ms. Buffer absorbs the burst, ticker
  drains it, encoded silence pads underruns (`0xD5` A-law, `0xFF` mu-law). The buffer is
  capped at 2 s and drops the **oldest** bytes on overflow to bound latency. Changing the
  write cadence to "whenever audio arrives" produces choppy garbage that sounds like a
  network problem.
- **`realtime.Dial` blocks on the `session.updated` echo** and fails the call if the audio
  format came back different from what was requested. This is not defensive padding: there
  is a known failure mode where the session silently reverts to `pcm16`, and streaming
  G.711 into it yields loud static rather than any error. Do not relax this check.
- **Codecs are pinned** via `answerCodecs` in `internal/telephony`. Adding G.722 or Opus
  breaks the passthrough. `telephone-event` must stay in the list or RFC 2833 DTMF is lost.
- **RTP port range is a package-level global** in diago (`media.RTPPortStart/End`), not a
  per-instance option — it is set once in `NewAgent`.
- **`diago.AudioStereoRecordingWav` contains a mutex** and must never be copied; `go vet`
  catches this. `startRecording` keeps it addressable and returns only its reader/writer
  plus a close func.
- Registration runs in a retry loop with exponential backoff (1s → 60s) so a Fritz!Box
  reboot does not take the service down.
- Notification is best-effort and must never fail a call: the transcript lands on disk
  before any network delivery is attempted.

**Secrets are logged as fingerprints, never in full** (`config.Fingerprint`). The point
is that a mismatch between the intended and the effective credential must be diagnosable
from the log without the secret ever appearing in it.

**Credentials are checked at startup, not mid-call.** `realtime.Preflight` verifies the
API key before registering, because the Realtime session is otherwise only established
once a call is already up — so a bad key presents as a caller hearing ringing and then a
dead line. A rejected key is fatal (refusing to start lets the Fritz!Box answering
machine take calls instead); an unreachable API is only a warning, since it may well be
back before anyone rings.

**Audio faults are diagnosed from the `call audio quality` line, not the transcript.**
The transcript records what the model said, so it looks perfect even when the caller
heard stutters. `starved_frames` (padding *during* an utterance) means the model could
not keep up with the RTP clock; `self_barge_ins` means the handset echoed the assistant
back and server VAD cut it off mid-sentence. Those two have opposite fixes, which is why
they are counted separately.

**The WAV is tapped before the wire.** `AudioStereoRecordingWav` wraps the reader and
writer inside this process, and diago's monitor injects silence for write gaps over
40 ms, so the recording reflects both content and cadence as this process produced them
-- but says nothing about RTP delivery. A clean recording alongside choppy audio at the
handset localises the fault downstream; `logRTPStats` covers that half.

## Milestone 0: always verify the phone path first

`make echo` runs with no API key and echoes the caller's audio back to themselves. It
proves registration, SDP, codec negotiation and RTP in both directions with no LLM
involved. When debugging anything audio-related, go back to echo mode before suspecting
the model — it isolates the half of the system that is hardest to reason about.

## Gotchas

- **Use the Fritz!Box's IP as the registrar, never `fritz.box`.** `.box` is a public
  gTLD; when local DNS does not answer for it the name resolves to an internet host and
  REGISTER leaks the SIP username there, presenting as a bare `Timer_B` timeout.
  `telephony.ResolveRegistrar` refuses public addresses at startup for this reason.
- Fritz!Box IP-phone credentials must both be ≥ 8 chars and the password must differ
  substantially from the username, or the box silently refuses to save them.
- The host must hold an address inside the Fritz!Box's subnet.
- `PHONELLM_MAX_CALL_TIME` is a cost ceiling, not a nicety: Realtime audio runs roughly
  $0.10–0.30 per call-minute, and a stuck call would bill overnight.
- Outbound calling is deliberately not implemented. Add it as a method on the telephony
  agent, not by restructuring the bridge.
