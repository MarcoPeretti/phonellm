# phonellm

An LLM answering machine for a Fritz!Box line. One line, one user, no PBX.

`phonellm` registers with your Fritz!Box as an ordinary LAN IP telephone, answers the
numbers the box routes to it, and bridges the call audio straight to the OpenAI Realtime
API. A single static binary. Nothing is exposed to the internet.

## Why there is no PBX here

The Fritz!Box 7590 **is** the PBX. It registers up to ten SIP endpoints as
"LAN/Wi-Fi (IP telephone)" and lets you choose, per endpoint, which numbers it reacts to
for incoming calls. FreePBX and Asterisk add nothing to a one-line, one-user setup.

The second simplification: the Fritz!Box negotiates G.711 on internal legs, and the
Realtime API accepts `audio/pcma` / `audio/pcmu` directly. So the media path is a **byte
passthrough** — RTP payload → base64 → WebSocket and back. No transcoding, no
resampling, no DSP code, and no added latency.

```
Caller ──PSTN──► Fritz!Box 7590            registrar, routes the number
                     │  SIP + RTP over LAN, G.711 8 kHz, 20 ms frames
                     ▼
              phonellm (this binary)       sipgo + diago
                     │  WSS, audio/pcma base64
                     ▼
              OpenAI Realtime (gpt-realtime)
```

## Setup

### 1. Create the IP telephone in the Fritz!Box

Telephony → Telephony Devices → Configure New Device → **Telephone** →
**LAN/Wi-Fi (IP telephone)**.

- Username and password must both be **at least 8 characters**, and the password must
  differ substantially from the username, or the box refuses to save.
- Assign which incoming numbers this device should react to.
- The host running `phonellm` must have an address in the Fritz!Box's subnet. Give it a
  static lease.

> **Use the box's IP address, not `fritz.box`.** `.box` is a real public gTLD, so unless
> your local DNS answers for `fritz.box`, the name resolves to a stranger's host on the
> internet and your REGISTER — carrying your SIP username — goes to them. The symptom is
> a bare `Timer_B timed out` that looks like a SIP fault. `phonellm` refuses to start if
> the registrar resolves outside the private ranges; override with
> `PHONELLM_ALLOW_PUBLIC_REGISTRAR=true` only for a genuine external SIP provider.

### 2. Prove the phone path before involving an LLM

This is the gate. Do not skip it.

```sh
cp .env.example .env && $EDITOR .env      # SIP credentials only; no API key needed
set -a && . ./.env && set +a
make echo
```

Call the line. You should hear **yourself**, echoed back. That proves registration, SDP,
codec negotiation and RTP in both directions. If this does not work, no amount of LLM
configuration will help.

### 3. Run it for real

Set `OPENAI_API_KEY`, flip `PHONELLM_ECHO_TEST=false`, and `make run`.

### 4. Install as a service

```sh
make build-linux-arm64                       # or build-linux-amd64
sudo install -m 0755 bin/phonellm-linux-arm64 /usr/local/bin/phonellm
sudo install -d -m 0750 /etc/phonellm
sudo install -m 0600 .env /etc/phonellm/phonellm.env
sudo install -m 0644 deploy/phonellm.service /etc/systemd/system/
sudo useradd --system --no-create-home phonellm
sudo systemctl enable --now phonellm
```

Open the firewall for SIP `5060/udp` and the RTP range (`16384-16484/udp` by default).

**Keep the Fritz!Box's own answering machine armed** with a delay longer than
`PHONELLM_RING_DELAY`. If `phonellm` is down, the box picks up instead of the caller
hearing endless ringing.

## Configuration

Everything is environment-driven; see [.env.example](.env.example) for the full list
with defaults. The ones that matter most:

| Variable | Default | Notes |
|---|---|---|
| `PHONELLM_SIP_USER` / `_PASS` | — | required; from the Fritz!Box UI |
| `PHONELLM_SIP_REGISTRAR` | `192.168.1.1` | use the IP, not `fritz.box` |
| `PHONELLM_BIND_HOST` | auto | auto-detected from the route to the registrar |
| `PHONELLM_ECHO_TEST` | `false` | echo mode; needs no API key |
| `PHONELLM_RING_DELAY` | `2s` | leaves room for a human to pick up first |
| `PHONELLM_MAX_CALL_TIME` | `5m` | hard cost ceiling per call |
| `PHONELLM_SILENCE_TIMEOUT` | `30s` | hangs up on a silent caller |
| `PHONELLM_INSTRUCTIONS_FILE` | — | the assistant's persona and house rules |

## Layout

| Path | Responsibility |
|---|---|
| `cmd/phonellm` | wiring, config load, LAN address detection, signals |
| `internal/telephony` | registration, answering, codec pinning, recording |
| `internal/realtime` | Realtime WebSocket client and session setup |
| `internal/bridge` | the two audio pumps, output pacing, barge-in, transcript |
| `internal/notify` | transcript storage, summary, Telegram/email delivery |

## Design notes worth knowing before you change anything

**The output pacer is the load-bearing part.** The model emits audio in bursts; RTP
demands exactly one 160-byte frame every 20 ms. `internal/bridge` buffers the bursts and
a ticker writes one frame per tick, padding with encoded silence on underrun (`0xD5` for
A-law, `0xFF` for mu-law). Deviating from that cadence is what turns natural speech into
choppy garbage. The buffer is capped at two seconds and drops the *oldest* audio on
overflow, so latency stays bounded instead of drifting.

**The session format is asserted, not assumed.** There is a known failure mode where
`session.update` silently falls back to `pcm16`. Streaming G.711 into a PCM session
produces loud static rather than an error, so `realtime.Dial` blocks on the
`session.updated` echo and refuses the call if the format came back wrong.

**Codecs are pinned to G.711** in the SDP answer. If G.722 were selected the passthrough
property would quietly stop holding. `telephone-event` is kept in the offer so RFC 2833
DTMF still arrives if you later want "press 1" handling.

**Barge-in** flushes the output buffer and sends `response.cancel` on
`input_audio_buffer.speech_started`, so the assistant goes quiet within one frame rather
than talking over the caller.

**Outbound calls** are not implemented. The SIP layer is structured so adding them is a
new method on the agent rather than a restructure.

## Cost

`gpt-realtime` runs roughly **$0.10–0.30 per call-minute** at current audio token rates.
`PHONELLM_MAX_CALL_TIME` is a hard ceiling so a stuck call cannot run up a bill
overnight; `gpt-realtime-mini` cuts it further.

## Licence

The SIP and media stack ([diago](https://github.com/emiago/diago),
[sipgo](https://github.com/emiago/sipgo)) is MPL-2.0.
