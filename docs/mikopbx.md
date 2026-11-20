# Connecting MikoPBX to audiosocket-bridge

The bridge exposes a plain TCP AudioSocket endpoint. Asterisk (MikoPBX) dials
it from the dialplan with the `AudioSocket()` application.

```
exten => 7000,1,Answer()
 same => n,AudioSocket(<uuid>,<host>:<port>)
 same => n,Hangup()
```

## 1. Network

The listener is intentionally not published on the host. MikoPBX must reach the
container over a Docker network.

**Same Docker network (recommended).** Attach the MikoPBX container to the
network created by this project:

```sh
docker network connect audiosocket <mikopbx-container>
```

Or, when the network already exists, start the bridge on it:

```sh
BRIDGE_NETWORK_EXTERNAL=true BRIDGE_NETWORK_NAME=mikopbx docker compose up -d
```

Verify from inside the MikoPBX container:

```sh
docker exec -it <mikopbx-container> sh -c 'nc -z -w2 bridge 9092 && echo reachable'
```

**Separate hosts.** Do not publish `9092` to the internet. Use a private
network path instead: WireGuard/Tailscale, a Docker macvlan network, or an SSH
tunnel. The protocol has no authentication, so network isolation is the only
access control.

## 2. Enable the AudioSocket application

AudioSocket ships with Asterisk 13+ as `app_audiosocket.so` and is present in
current MikoPBX builds. Confirm inside the MikoPBX container:

```sh
asterisk -rx "core show application AudioSocket"
```

If it is missing, load it:

```sh
asterisk -rx "module load app_audiosocket.so"
```

Persist the load in `/etc/asterisk/modules.conf` with `load => app_audiosocket.so`
if your build does not autoload it.

## 3. Dialplan

In MikoPBX, custom dialplan lives in `/etc/asterisk/extensions_custom.conf`
(editable from the web UI under **System → Custom files**, or directly in the
container). Add a context and include it where you need it:

```
[audiosocket-bridge]
exten => 7000,1,Answer()
 same => n,NoOp(AudioSocket call ${CHANNEL(uniqueid)})
 same => n,AudioSocket(409c8f2a-1122-3344-5566-778899aabbcc,bridge:9092)
 same => n,Hangup()
```

Notes:

- The first argument must be a UUID string. A fixed UUID per extension is fine;
  the bridge uses it for logging and correlation only. Generate one with
  `uuidgen` if you want a unique value per route.
- `bridge:9092` is the container name and the internal port. Replace `bridge`
  with the service name or IP that resolves inside your Docker network.
- `Answer()` is required before `AudioSocket()` so the caller hears the audio
  path immediately.
- Point an extension, IVR entry, or inbound route at this context to place the
  call.

## 4. Verify

Watch the bridge log:

```sh
docker compose logs -f bridge
```

A healthy call looks like:

```json
{"level":"INFO","msg":"call started","uuid":"409c8f2a-...","endpoint":"echo"}
{"level":"INFO","msg":"call ended","reason":"hangup","frames_in":1500,"frames_out":1500,...}
```

In echo mode the caller hears their own voice with a short delay. If you hear
nothing, check in this order:

1. `docker compose exec bridge wget -qO- http://127.0.0.1:8080/stats` — was a
   call accepted?
2. `/stats` counters: `accepted` increments on a valid UUID, `rejected` on a
   busy bridge, `failed` on a handshake or backend error.
3. `/stats` → `active_call.in_frames` — if this stays at zero, Asterisk is not
   sending audio (check `Answer()`, codec, and the AudioSocket module).
4. `frames_in` > 0 but `frames_out` == 0 — the endpoint is not producing audio
   (virtual mode: check `endpoint_stats.last_error` and the audio service).

The tester can validate the bridge independently of Asterisk:

```sh
docker compose exec bridge audiosocket-tester -addr 127.0.0.1:9092 -duration 3s
```

## 5. Busy behaviour

Only one call is bridged. A second caller gets an AudioSocket error frame with
`busy` and is disconnected, so Asterisk hangs up that channel. The rejection is
counted in `/stats` under `counters.rejected`.

If MikoPBX retries aggressively, put the AudioSocket call behind a queue or an
IVR announcement rather than dialling it directly.
