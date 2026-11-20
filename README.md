# audiosocket-bridge

Lightweight Go service that connects Asterisk / MikoPBX to Linux virtual audio
devices over the AudioSocket protocol. It speaks the official framing protocol,
carries bidirectional 16-bit signed linear PCM, and can either echo audio back
to the caller or bridge it into a PulseAudio / PipeWire virtual microphone and
speaker pair.

Only one call is active at a time. A second caller receives an AudioSocket
error frame and is disconnected. The AudioSocket listener is never published
outside the Docker network.

## What it does

```
Asterisk / MikoPBX                         audiosocket-bridge
┌──────────────────┐   TCP 9092   ┌──────────────────────────────────────┐
│ AudioSocket(uuid,│─────────────▶│ frame reader → session → endpoint     │
│  bridge:9092)    │◀─────────────│ endpoint → outbound queue → writer    │
└──────────────────┘  slin PCM    └───────────────┬──────────────────────┘
                                                  │
                        echo mode ────────────────┘  (byte-for-byte return)

                        virtual mode
                        ┌──────────────────────────────────────────────┐
                        │ pacat → audiosocket_mic (sink)               │
                        │          └ .monitor → remap-source           │
                        │                "AudioSocket_Mic" (Chromium)  │
                        │                                              │
                        │ parec ← audiosocket_spk.monitor              │
                        │      ← Chromium voice output                 │
                        └──────────────────────────────────────────────┘
```

Properties:

- Official AudioSocket framing: `kind` (1 byte), big-endian payload length
  (2 bytes), payload. Matches Asterisk's `include/asterisk/res_audiosocket.h`.
- Frame kinds:

  | Kind | Meaning | Direction |
  | --- | --- | --- |
  | `0x00` | hangup (empty payload) | bridge → Asterisk |
  | `0x01` | UUID, 16 raw bytes | Asterisk → bridge |
  | `0x03` | DTMF digit, 1 ASCII byte | Asterisk → bridge |
  | `0x10` | signed linear 16-bit mono PCM, 8 kHz | both |
  | `0x11`–`0x18` | same, 12/16/24/32/44.1/48/96/192 kHz | both |
  | `0xFF` | error, optional error code | bridge → Asterisk |

  The outbound audio kind follows `BRIDGE_CALL_RATE`. Audio frames at a rate
  that was not negotiated are counted and dropped rather than mixed into the
  call. Unknown kinds are counted and ignored.
- Partial TCP reads, coalesced frames, zero-length frames and oversize frames
  are handled explicitly.
- Audio is 8 kHz mono s16le by default. Other rates are configurable.
- Echo mode forwards each inbound frame immediately: one goroutine hand-off,
  no buffering, no resampling, no level change.
- Virtual mode paces outbound audio at one frame per frame interval, absorbs
  jitter in a bounded buffer, and fills underruns with silence so the Asterisk
  stream stays continuous.
- Bounded queues drop the oldest audio on overflow, so latency cannot grow
  without limit. Drops are counted, not logged per frame.
- Structured JSON logging. Raw audio is never logged or written to disk.
- Graceful shutdown: stop accepting, cancel the call, close the socket, wait
  with a timeout.
- Health, readiness, JSON statistics and Prometheus metrics over HTTP.

## Quick start (echo mode)

```sh
go build -o bin/audiosocket-bridge ./cmd/audiosocket-bridge
go build -o bin/audiosocket-tester ./cmd/audiosocket-tester

BRIDGE_MODE=echo ./bin/audiosocket-bridge
```

In another shell, verify the protocol end to end without a phone call:

```sh
./bin/audiosocket-tester -addr 127.0.0.1:9092 -duration 3s
# frames sent=150 received=150 lost=0 ... OK
```

Check health and statistics:

```sh
curl -s localhost:8080/healthz   # ok
curl -s localhost:8080/readyz    # {"mode":"echo","ready":true,"reason":""}
curl -s localhost:8080/stats     # JSON snapshot
curl -s localhost:8080/metrics   # Prometheus text
```

## Operating modes

### `echo`

Returns inbound PCM to Asterisk unchanged. Use it to prove the
FreeCall → MikoPBX → AudioSocket path before touching the audio stack. Audio
integrity is exact: the same bytes that arrive are framed and sent back.

### `virtual`

Routes audio through PulseAudio or PipeWire using `pacat` and `parec`:

- Inbound phone audio → `BRIDGE_PA_PLAYBACK_SINK` (`audiosocket_mic`). Its
  monitor is remapped to the source `audiosocket_mic_source`, which is the
  microphone Chromium will capture from.
- Chromium voice output → `audiosocket_spk` sink → captured from
  `audiosocket_spk.monitor` → resampled if needed → paced back to Asterisk.

Both processes are supervised with exponential backoff and reported in
statistics and readiness. If the device is opened at the call rate (default),
PulseAudio performs the sample-rate conversion. Set
`BRIDGE_PA_DEVICE_RATE=48000` only when the device is fixed-rate; the bridge
then converts internally.

See [docs/audio-pipeline.md](docs/audio-pipeline.md) for device setup,
latency budget and the future Chromium wiring.

## Configuration

Environment only. All values can live in `.env`; see `.env.example`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIDGE_MODE` | `echo` | `echo` or `virtual` |
| `BRIDGE_LISTEN_ADDR` | `:9092` | AudioSocket TCP listener |
| `BRIDGE_HEALTH_ADDR` | `:8080` | Health/metrics listener, empty disables |
| `BRIDGE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `BRIDGE_LOG_FORMAT` | `json` | `json` or `text` |
| `BRIDGE_CALL_RATE` | `8000` | Call sample rate in Hz |
| `BRIDGE_FRAME_MS` | `20ms` | Outbound frame duration |
| `BRIDGE_MAX_FRAME_BYTES` | `4096` | Largest accepted payload |
| `BRIDGE_READ_BUFFER_BYTES` | `8192` | `bufio` reader size |
| `BRIDGE_IN_BUFFER_MS` | `500` | Call → device smoothing buffer |
| `BRIDGE_OUT_QUEUE_FRAMES` | `50` | Device → call queue depth (1 s) |
| `BRIDGE_UUID_TIMEOUT` | `3s` | Deadline for the first frame |
| `BRIDGE_READ_TIMEOUT` | `2m` | Idle read deadline, `0` disables |
| `BRIDGE_WRITE_TIMEOUT` | `10s` | Socket write deadline |
| `BRIDGE_MAX_CALL_DURATION` | `4h` | Hard call limit, `0` disables |
| `BRIDGE_SHUTDOWN_TIMEOUT` | `10s` | Graceful shutdown budget |
| `BRIDGE_COLLECT_AUDIO_STATS` | `true` | RMS/peak metering (no audio kept) |
| `BRIDGE_PA_PLAYBACK_SINK` | `audiosocket_mic` | Sink for phone audio |
| `BRIDGE_PA_CAPTURE_SOURCE` | `audiosocket_spk.monitor` | Source to capture |
| `BRIDGE_PA_DEVICE_RATE` | `8000` | Device rate; differs → internal SRC |
| `BRIDGE_PA_DEVICE_CHANNELS` | `1` | Device channels |
| `BRIDGE_PA_LATENCY` | `40ms` | `pacat`/`parec` latency request |
| `BRIDGE_PA_OUT_BUFFER_MS` | `400` | Device → call jitter buffer |
| `BRIDGE_PA_SILENCE_ON_UNDERRUN` | `true` | Send silence on capture gaps |
| `BRIDGE_PA_PACAT_BIN` | `pacat` | Playback binary |
| `BRIDGE_PA_PAREC_BIN` | `parec` | Capture binary |
| `BRIDGE_PA_RESTART_INITIAL` | `500ms` | First restart delay |
| `BRIDGE_PA_RESTART_MAX` | `30s` | Maximum restart delay |
| `BRIDGE_PA_MAX_RESTARTS` | `10` | Consecutive failures before unready |

## Docker and Coolify

```sh
docker compose up -d --build
docker compose logs -f bridge
```

The compose file runs two services on a private network named `audiosocket`:

- `bridge` – the Go service. It publishes **no** ports; `9092` is only on the
  Docker network. Its health endpoint is reachable inside the network.
- `audio` – a small PulseAudio server with the two null sinks and the remapped
  microphone source, sharing a named volume `pulse`.

For Coolify:

1. Create a resource from this repository using the Docker Compose build pack.
2. Keep `BRIDGE_MODE=echo` for the first deployment and set the other variables
   from `.env.example`.
3. Do not add a port mapping for `9092`. Coolify can use the container
   healthcheck on `/healthz` (port `8080`).
4. Attach the `audiosocket` network to the MikoPBX container, or set
   `BRIDGE_NETWORK_EXTERNAL=true` and `BRIDGE_NETWORK_NAME=<existing network>`.
5. Point the MikoPBX dialplan at `bridge:9092`.

The bridge only accepts AudioSocket connections. It has no public surface, no
TLS and no authentication, so the network boundary is the security boundary.
Keep `9092` off host port mappings and off the public internet.

## MikoPBX

See [docs/mikopbx.md](docs/mikopbx.md) for the dialplan, network wiring and
verification steps.

Short version, in `extensions_custom.conf`:

```
[audiosocket-bridge]
exten => 7000,1,Answer()
 same => n,AudioSocket(409c8f2a-1122-3344-5566-778899aabbcc,bridge:9092)
 same => n,Hangup()
```

## Health and observability

| Endpoint | Purpose |
| --- | --- |
| `/healthz` | Liveness, always `200` while the process runs |
| `/readyz` | `200` when accepting calls and the backend is healthy, else `503` |
| `/stats` | JSON: uptime, counters, active call, endpoint details |
| `/metrics` | Prometheus text: readiness, calls, frames, drops, underruns |

The active call snapshot includes frame and byte counts per direction, dropped
frames, DTMF digits, and last RMS / peak level in dBFS. Levels are aggregates;
audio content is never stored.

## Security and privacy

- No raw call audio is logged, buffered to disk, or exported.
- Audio level metering is optional (`BRIDGE_COLLECT_AUDIO_STATS`).
- The listener has no authentication by design; it must stay on a trusted
  Docker network.
- The bundled PulseAudio socket uses `auth-anonymous=1` inside a private
  volume. That grants full audio access to anything that can mount the volume.
  Only share it with containers that need it.
- When using a host PulseAudio socket, prefer mounting it read-only and running
  the container with your UID over enabling an anonymous TCP socket.

## Extending to ChatGPT Voice

`internal/bridge` is the seam. The AudioSocket server, protocol handling,
session lifecycle and statistics only depend on the `bridge.Endpoint`
interface:

```go
type Endpoint interface {
    Name() string
    Start(ctx context.Context, sink Sink) error
    Write(pcm []byte) error
    Stats() Stats
    Close() error
}
```

A future `chromium` endpoint can write call audio into a virtual microphone,
capture the assistant's reply, and reuse the same pacing and framing code. No
change to `internal/audiosocket` is required.

## Development

```sh
go test ./...        # protocol, buffers, resampler, session and shutdown tests
go vet ./...
go build ./...
```

Layout:

```
cmd/audiosocket-bridge    service entry point
cmd/audiosocket-tester    protocol and echo verification client
internal/audiosocket      framing, session, single-call server
internal/bridge           Endpoint interface, echo and virtual backends
internal/audio            PCM helpers, bounded buffers, frame queue
internal/audio/resample   streaming band-limited resampler
internal/config           environment configuration and validation
internal/health           health, stats and metrics HTTP endpoints
internal/logging          structured logger and subprocess log capture
docker/audio              PulseAudio sidecar with virtual devices
```
