#!/usr/bin/env bash
# Creates the virtual devices the bridge expects on a host running
# PulseAudio or PipeWire (with pipewire-pulse).
#
# Run as the desktop user that owns the audio session, not as root.
set -euo pipefail

MIC_SINK=${MIC_SINK:-audiosocket_mic}
SPK_SINK=${SPK_SINK:-audiosocket_spk}
MIC_SOURCE=${MIC_SOURCE:-audiosocket_mic_source}

echo "creating null sinks and remapped microphone source"
pactl load-module module-null-sink \
  sink_name="$MIC_SINK" \
  sink_properties=device.description=AudioSocket_Mic >/dev/null

pactl load-module module-null-sink \
  sink_name="$SPK_SINK" \
  sink_properties=device.description=AudioSocket_Spk >/dev/null

pactl load-module module-remap-source \
  master="$MIC_SINK.monitor" \
  source_name="$MIC_SOURCE" \
  source_properties=device.description=AudioSocket_Mic >/dev/null

echo
echo "devices:"
pactl list short sinks | grep -E "$MIC_SINK|$SPK_SINK" || true
pactl list short sources | grep -E "$MIC_SINK|$MIC_SOURCE|$SPK_SINK" || true

cat <<'EOF'

The bridge needs access to your PulseAudio socket. Two options:

1. Recommended: mount the socket and match the UID.
   docker run --user "$(id -u):$(id -g)" \
     -e PULSE_SERVER=unix:/pulse/native \
     -v "${XDG_RUNTIME_DIR}/pulse:/pulse" ...

2. Anonymous socket (weaker isolation, only for a private directory):
   pactl load-module module-native-protocol-unix \
     socket=/run/user/$(id -u)/audiosocket-pulse auth-anonymous=1
   Then mount /run/user/$(id -u)/audiosocket-pulse into the container.

These modules are not persistent. Re-run this script after each login, or add
the three load-module lines to ~/.config/pulse/default.pa.
EOF
