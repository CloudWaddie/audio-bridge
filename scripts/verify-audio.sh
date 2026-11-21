#!/bin/sh
# End-to-end verification for the ChatGPT Voice stack, run on the deploy
# host (Linux, Docker). Proves the audio path and the controller state
# without placing a real phone call.
#
#   1. PulseAudio devices and pinned defaults
#   2. AudioSocket loop: the tester places a "call"; a sine standing in for
#      the browser's voice output is played into audiosocket_spk; the
#      returning frames must carry that tone to the caller.
#   3. Phone -> browser mic path: sine into the sink the bridge plays into,
#      measured at the source the browser records from.
#   4. Controller state (login + readiness).
set -eu

COMPOSE=${COMPOSE:-docker compose}
TONE_S=3
fails=0

check() {
    if [ "$1" = "0" ]; then
        echo "PASS: $2"
    else
        echo "FAIL: $2"
        fails=$((fails + 1))
    fi
}

rms_dbfs() {
    # RMS level in dBFS of $1 seconds of s16le mono captured to /tmp/cap.raw
    $COMPOSE exec -T audio python3 - "$1" <<'EOF'
import math, struct, sys

seconds = float(sys.argv[1])
data = open("/tmp/cap.raw", "rb").read()
n = len(data) // 2
want = int(48000 * 2 * seconds)
if n < want // 2:
    sys.exit("no audio captured")
samples = struct.unpack("<%dh" % n, data[: n * 2])
rms = (sum((s / 32768.0) ** 2 for s in samples) / n) ** 0.5
print(f"  level: {20 * math.log10(rms):.1f} dBFS")
sys.exit(0 if rms > 0.01 else "captured audio is silent")
EOF
}

capture() {
    # parec from $1 for $2 s into /tmp/cap.raw
    $COMPOSE exec -T audio sh -c "
        parec --device='$1' --format=s16le --rate=48000 --channels=1 --raw 2>/dev/null \
            | head -c $((48000 * 2 * $2)) > /tmp/cap.raw
    "
}

echo "== 1. PulseAudio devices and defaults"
$COMPOSE exec -T audio pactl set-default-sink audiosocket_spk 2>/dev/null || true
$COMPOSE exec -T audio pactl set-default-source audiosocket_mic_source 2>/dev/null || true
$COMPOSE exec -T audio pactl info | sed -n 's/^Default Sink: /  default sink: /p; s/^Default Source: /  default source: /p'
$COMPOSE exec -T audio pactl list short sinks | grep -q audiosocket_mic
check $? "audiosocket_mic sink exists"
$COMPOSE exec -T audio pactl list short sinks | grep -q audiosocket_spk
check $? "audiosocket_spk sink exists"
$COMPOSE exec -T audio pactl list short sources | grep -q audiosocket_mic_source
check $? "audiosocket_mic_source (browser mic) exists"
$COMPOSE exec -T audio pactl info | grep -q "Default Sink: audiosocket_spk"
check $? "default sink is audiosocket_spk"
$COMPOSE exec -T audio pactl info | grep -q "Default Source: audiosocket_mic_source"
check $? "default source is audiosocket_mic_source"

echo "== 2. AudioSocket loop with browser-audio substitute (${TONE_S}s)"
# A sine into audiosocket_spk stands in for ChatGPT's voice output. The
# tester places a call: audio it gets back must be the sine, proving
# call -> bridge -> (would-be browser) -> bridge -> caller.
SINE_IDX=$($COMPOSE exec -T audio sh -c "pactl load-module module-sine sink=audiosocket_spk frequency=440 && pactl list short modules | awk '/module-sine/{print \$1; exit}'")
$COMPOSE exec -T bridge audiosocket-tester -addr bridge:9092 -duration "${TONE_S}s" \
    | tee /tmp/as-tester.log
$COMPOSE exec -T audio pactl unload-module "$SINE_IDX" || true
grep -q "^OK$" /tmp/as-tester.log
check $? "call completed and returned audio"
grep -q "last returned frame level" /tmp/as-tester.log
check $? "returned audio carried the sine (not silence)"

echo "== 3. Phone -> browser mic path (tone at the call sink, measured at browser mic)"
SINE_IDX=$($COMPOSE exec -T audio sh -c "pactl load-module module-sine sink=audiosocket_mic frequency=440 && pactl list short modules | awk '/module-sine/{print \$1; exit}'")
capture audiosocket_mic_source "$TONE_S"
$COMPOSE exec -T audio pactl unload-module "$SINE_IDX" || true
rms_dbfs "$TONE_S"
check $? "caller audio reaches the browser microphone source"

echo "== 4. Controller state"
$COMPOSE exec -T bridge wget -qO- http://camoufox:8080/statusz | sed 's/^/  /'
$COMPOSE exec -T bridge wget -qO- http://camoufox:8080/statusz | grep -q '"logged_in": true'
check $? "camoufox reports logged_in"

echo
if [ "$fails" = "0" ]; then
    echo "ALL CHECKS PASSED"
else
    echo "$fails CHECK(S) FAILED"
    exit 1
fi
