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

exec pulseaudio \
  --daemonize=false \
  --disallow-exit \
  --exit-idle-time=-1 \
  --log-target=stderr \
  -n \
  --load="module-native-protocol-unix socket=${SOCKET_DIR}/native auth-anonymous=1" \
  --load="module-null-sink sink_name=audiosocket_mic sink_properties=device.description=AudioSocket_Mic" \
  --load="module-null-sink sink_name=audiosocket_spk sink_properties=device.description=AudioSocket_Spk" \
  --load="module-remap-source master=audiosocket_mic.monitor source_name=audiosocket_mic_source source_properties=device.description=AudioSocket_Mic" \
  --load="module-always-sink"
