#!/bin/sh
# Starts a private PulseAudio server that exposes two virtual devices:
#
#   audiosocket_mic          sink the bridge plays phone audio into
#   audiosocket_mic_source   remapped source (monitor of the sink) for Chromium
#   audiosocket_spk          sink Chromium plays its voice output into
#   audiosocket_spk.monitor  source the bridge captures
#
# The unix socket is shared through the "pulse" volume, so the bridge and any
# future Chromium container can use the same server.
set -eu

RUNTIME_DIR="${XDG_RUNTIME_DIR:-/tmp/pulse-runtime}"
mkdir -p "$RUNTIME_DIR"
chmod 700 "$RUNTIME_DIR"

SOCKET_DIR="${PULSE_SOCKET_DIR:-/pulse}"
mkdir -p "$SOCKET_DIR"

pulseaudio \
  --daemonize=false \
  --disallow-exit \
  --exit-idle-time=-1 \
  --log-target=stderr \
  -n \
  --load="module-native-protocol-unix socket=${SOCKET_DIR}/native auth-anonymous=1" \
  --load="module-null-sink sink_name=audiosocket_mic sink_properties=device.description=AudioSocket_Mic" \
  --load="module-null-sink sink_name=audiosocket_spk sink_properties=device.description=AudioSocket_Spk" \
  --load="module-remap-source master=audiosocket_mic.monitor source_name=audiosocket_mic_source source_properties=device.description=AudioSocket_Mic" \
  --load="module-always-sink" &
PA_PID=$!

# The protocol socket appears before the sink and source modules finish
# loading, so setting the defaults at socket time races and can silently
# fail. Wait until the server answers AND every virtual device exists.
i=0
while [ $i -lt 100 ] && ! pactl info >/dev/null 2>&1; do
  sleep 0.1
  i=$((i + 1))
done
i=0
while [ $i -lt 100 ]; do
  if pactl list short sinks 2>/dev/null | grep -q audiosocket_spk \
     && pactl list short sources 2>/dev/null | grep -q audiosocket_mic_source; then
    break
  fi
  sleep 0.1
  i=$((i + 1))
done

# Pin the defaults so browser clients land on the right devices without
# device pickers: Firefox output goes to audiosocket_spk and its microphone
# capture comes from audiosocket_mic_source (the caller's voice).
if ! pactl set-default-sink audiosocket_spk 2>/dev/null; then
  echo "warning: could not set default sink" >&2
fi
if ! pactl set-default-source audiosocket_mic_source 2>/dev/null; then
  echo "warning: could not set default source" >&2
fi
pactl info 2>/dev/null | grep -E '^Default (Sink|Source)' || true

wait "${PA_PID}"
