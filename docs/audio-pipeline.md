# Virtual audio pipeline

The virtual mode connects a phone call to a pair of PulseAudio / PipeWire
devices. The same devices are meant to be used by a headless Chromium running
ChatGPT Voice later.

## Device topology

```
                       ┌──────────────────────────────┐
 call audio ── pacat ─▶│ audiosocket_mic (null sink)  │
                       └───────────────┬──────────────┘
                                       │ .monitor
                       ┌───────────────▼──────────────┐
                       │ audiosocket_mic_source       │  Chromium microphone
                       │ (remap-source)               │
                       └──────────────────────────────┘

                       ┌──────────────────────────────┐
 Chromium speaker ────▶│ audiosocket_spk (null sink)  │
                       └───────────────┬──────────────┘
                                       │ .monitor
 call audio ◀── parec ─────────────────┘
```

Why a remapped source: Chromium lists *sources* as microphones. A null sink's
monitor is a source, but many applications filter monitor sources out. A
`module-remap-source` presents a normal source named `AudioSocket_Mic`, which
Chromium accepts.

## Bundled audio service (Docker)

`docker-compose.yml` starts `audio`, an Alpine container running PulseAudio
with those modules loaded and a unix socket in the shared `pulse` volume.

Check it:

```sh
docker compose exec audio pactl list short sinks
docker compose exec audio pactl list short sources
```

Expected:

```
audiosocket_mic  ... AudioSocket_Mic
audiosocket_spk  ... AudioSocket_Spk
audiosocket_mic_source ... AudioSocket_Mic
audiosocket_spk.monitor ...
```

## Host PulseAudio or PipeWire

If you prefer the host audio server, run `scripts/pulse-setup.sh` as your
desktop user, then mount the native socket into the bridge container:

```yaml
services:
  bridge:
    volumes:
      - ${XDG_RUNTIME_DIR}/pulse:/pulse
    environment:
      PULSE_SERVER: unix:/pulse/native
    user: "${UID}:${GID}"
```

Remove the bundled `audio` service and its `depends_on` in that case.

PipeWire works through `pipewire-pulse`, which provides `pacat` and `parec`.

> Security: `auth-anonymous=1` on a unix socket lets any process that can open
> the socket record and play audio. Keep the socket in a private volume or a
> `0700` directory, and never expose PulseAudio over TCP.

## Future Chromium integration

Chromium should run as another container that mounts the same `pulse` volume:

```yaml
  chromium:
    image: your-chromium-image
    environment:
      PULSE_SERVER: unix:/pulse/native
    volumes:
      - pulse:/pulse
    networks: [voice]
```

Then:

- Select `AudioSocket_Mic` as the microphone (it carries the caller's voice).
- Select `audiosocket_spk` as the output device (the bridge captures it).
- Disable audio processing that would distort telephony audio:
  `--disable-features=AudioServiceOutOfProcess` is not required, but
  `--use-fake-device-for-media-stream` must **not** be used.
- Allow autoplay for the voice page:
  `--autoplay-policy=no-user-gesture-required`.

The bridge does not depend on Chromium: in virtual mode it already provides the
device pair, so the ChatGPT integration can be added as a new
`internal/bridge` endpoint without touching AudioSocket handling.

## Latency budget

Virtual mode adds the following one-way delays (defaults):

| Stage | Typical |
| --- | --- |
| `pacat` device latency | 20–40 ms |
| PulseAudio null sink | ~5 ms |
| `parec` chunk (20 ms) | 0–20 ms |
| Bridge jitter buffer | 20–60 ms |
| Outbound pacing frame | 0–20 ms |

Expect roughly 80–150 ms one way. Echo mode adds less than a millisecond plus
the network. Reduce `BRIDGE_PA_LATENCY` and `BRIDGE_PA_OUT_BUFFER_MS` to trade
stability for latency; increase them if you hear dropouts.

## Sample rates

Default: the bridge opens the device at 8 kHz (`BRIDGE_PA_DEVICE_RATE=8000`) and
lets PulseAudio resample. This uses the server's high-quality resampler and
keeps the Go path allocation-free.

If a device is fixed at another rate, set `BRIDGE_PA_DEVICE_RATE` to that rate.
The bridge then converts with its own streaming band-limited resampler in both
directions. Internal conversion is designed for speech and is not bit-exact.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| `parec: command not found` | install `pulseaudio-utils` in the image |
| `Connection failure: Connection refused` | `PULSE_SERVER` and the `pulse` volume |
| No `audiosocket_mic_source` | `module-remap-source` loaded? `pactl list short sources` |
| Caller hears nothing | `/stats` → `active_call.endpoint_stats.last_error` |
| Choppy audio | raise `BRIDGE_PA_OUT_BUFFER_MS` and `BRIDGE_PA_LATENCY` |
| `underruns` climbing | Chromium is not producing audio, or capture source is wrong |
| `restarts` climbing | the device disappeared; the bridge retries with backoff |
