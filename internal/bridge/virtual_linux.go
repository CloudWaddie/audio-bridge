//go:build linux

package bridge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/cloudwaddie/audiosocket-bridge/internal/audio"
	"github.com/cloudwaddie/audiosocket-bridge/internal/audio/resample"
	"github.com/cloudwaddie/audiosocket-bridge/internal/config"
	"github.com/cloudwaddie/audiosocket-bridge/internal/logging"
)

// virtual routes call audio through PulseAudio or PipeWire using the pacat and
// parec utilities. Inbound call audio is played into a null sink whose monitor
// is exposed as a microphone; outbound audio is captured from another sink's
// monitor.
//
//	call -> in buffer -> pacat -> audiosocket_mic (sink)
//	                                `-> .monitor -> remap-source -> Chromium mic
//
//	Chromium speaker -> audiosocket_spk (sink) -> .monitor -> parec
//	                            -> down buffer -> paced frames -> call
type virtual struct {
	cfg  config.Config
	vcfg config.VirtualConfig
	log  *slog.Logger
	c    counters

	frameDur   time.Duration
	frameBytes int

	in   *audio.Buffer
	out  *audio.Buffer
	up   *resample.Resampler
	down *resample.Resampler

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	pacat   *exec.Cmd
	closing bool
}

func newVirtual(cfg config.Config, log *slog.Logger) (Endpoint, error) {
	v := &virtual{
		cfg:        cfg,
		vcfg:       cfg.Virtual,
		log:        log,
		frameDur:   cfg.FrameDuration,
		frameBytes: cfg.FrameBytes(),
		in:         audio.NewBuffer(cfg.InBufferBytes()),
		out:        audio.NewBuffer(cfg.OutBufferBytes()),
		up:         resample.New(cfg.CallRate, cfg.Virtual.DeviceRate),
		down:       resample.New(cfg.Virtual.DeviceRate, cfg.CallRate),
	}
	return v, nil
}

func preflightVirtual(cfg config.Config) error {
	if _, err := exec.LookPath(cfg.Virtual.PacatBin); err != nil {
		return fmt.Errorf("virtual audio mode needs %q in PATH: %w", cfg.Virtual.PacatBin, err)
	}
	if _, err := exec.LookPath(cfg.Virtual.ParecBin); err != nil {
		return fmt.Errorf("virtual audio mode needs %q in PATH: %w", cfg.Virtual.ParecBin, err)
	}
	return nil
}

func (v *virtual) Name() string { return "virtual" }

func (v *virtual) Start(parent context.Context, sink Sink) error {
	if sink == nil {
		return ErrNotStarted
	}
	if err := preflightVirtual(v.cfg); err != nil {
		return err
	}
	v.ctx, v.cancel = context.WithCancel(parent)
	v.c.ready.Store(true)

	v.wg.Add(3)
	go v.playbackLoop()
	go v.captureLoop()
	go v.pacerLoop(sink)

	v.log.Info("virtual audio started",
		"playback_sink", v.vcfg.PlaybackSink,
		"capture_source", v.vcfg.CaptureSource,
		"device_rate", v.vcfg.DeviceRate,
		"device_channels", v.vcfg.DeviceChannels,
		"resampling", !v.up.Passthrough(),
	)
	return nil
}

func (v *virtual) Write(pcm []byte) error {
	if v.ctx == nil {
		return ErrNotStarted
	}
	v.c.inFrames.Add(1)
	v.c.inBytes.Add(uint64(len(pcm)))
	if _, dropped := v.in.Write(pcm); dropped > 0 {
		v.c.dropped.Add(uint64(dropped))
	}
	return nil
}

// playbackLoop keeps pacat alive and feeds it call audio.
func (v *virtual) playbackLoop() {
	defer v.wg.Done()
	backoff := v.vcfg.RestartInitial
	for {
		if v.ctx.Err() != nil {
			return
		}
		cmd := exec.CommandContext(v.ctx, v.vcfg.PacatBin, v.playbackArgs()...)
		cmd.Stderr = logging.NewLineWriter(v.log, slog.LevelDebug, "pacat")
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 2 * time.Second

		stdin, err := cmd.StdinPipe()
		if err != nil {
			v.c.fail(err)
			return
		}
		if err := cmd.Start(); err != nil {
			v.c.fail(err)
			if !v.sleep(backoff) {
				return
			}
			backoff = nextBackoff(backoff, v.vcfg.RestartMax)
			continue
		}
		started := time.Now()
		v.setPacat(cmd)

		runErr := v.pumpToDevice(stdin)
		_ = stdin.Close()
		waitErr := cmd.Wait()
		v.setPacat(nil)

		if v.ctx.Err() != nil {
			return
		}
		if runErr == nil {
			runErr = waitErr
		}
		v.c.fail(runErr)
		if time.Since(started) > 5*time.Second {
			v.c.restarts.Store(0)
			backoff = v.vcfg.RestartInitial
			v.c.ready.Store(true)
		}
		v.c.restarts.Add(1)
		if int(v.c.restarts.Load()) >= v.vcfg.MaxRestarts {
			v.c.ready.Store(false)
		}
		v.log.Warn("playback process exited, restarting", "err", runErr, "restart_in", backoff)
		if !v.sleep(backoff) {
			return
		}
		backoff = nextBackoff(backoff, v.vcfg.RestartMax)
	}
}

func (v *virtual) pumpToDevice(w io.Writer) error {
	frame := make([]byte, v.frameBytes)
	samples := make([]int16, 0, v.frameBytes/2)
	devBytes := make([]byte, 0, v.frameBytes*2*v.vcfg.DeviceChannels)
	for {
		if _, err := v.in.ReadExact(v.ctx, frame); err != nil {
			return err
		}
		samples = audio.BytesToSamples(samples, frame)
		converted := v.up.Process(samples)
		devBytes = audio.MonoToChannels(devBytes[:0], converted, v.vcfg.DeviceChannels)
		if _, err := w.Write(devBytes); err != nil {
			return err
		}
	}
}

// captureLoop keeps parec alive and collects audio from the virtual speaker.
func (v *virtual) captureLoop() {
	defer v.wg.Done()

	chunkFrames := v.vcfg.DeviceRate / 50
	if chunkFrames < 1 {
		chunkFrames = 1
	}
	raw := make([]byte, chunkFrames*2*v.vcfg.DeviceChannels)
	mono := make([]int16, 0, chunkFrames)
	outBytes := make([]byte, 0, chunkFrames*2)
	backoff := v.vcfg.RestartInitial

	for {
		if v.ctx.Err() != nil {
			return
		}
		cmd := exec.CommandContext(v.ctx, v.vcfg.ParecBin, v.captureArgs()...)
		cmd.Stderr = logging.NewLineWriter(v.log, slog.LevelDebug, "parec")
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 2 * time.Second

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			v.c.fail(err)
			return
		}
		if err := cmd.Start(); err != nil {
			v.c.fail(err)
			if !v.sleep(backoff) {
				return
			}
			backoff = nextBackoff(backoff, v.vcfg.RestartMax)
			continue
		}
		started := time.Now()

		for {
			if _, err := io.ReadFull(stdout, raw); err != nil {
				break
			}
			mono = audio.ChannelsToMono(mono, raw, v.vcfg.DeviceChannels)
			converted := v.down.Process(mono)
			outBytes = audio.SamplesToBytes(outBytes[:0], converted)
			if _, dropped := v.out.Write(outBytes); dropped > 0 {
				v.c.dropped.Add(uint64(dropped))
			}
		}
		waitErr := cmd.Wait()
		if v.ctx.Err() != nil {
			return
		}
		v.c.fail(waitErr)
		if time.Since(started) > 5*time.Second {
			v.c.restarts.Store(0)
			backoff = v.vcfg.RestartInitial
			v.c.ready.Store(true)
		}
		v.c.restarts.Add(1)
		if int(v.c.restarts.Load()) >= v.vcfg.MaxRestarts {
			v.c.ready.Store(false)
		}
		v.log.Warn("capture process exited, restarting", "err", waitErr, "restart_in", backoff)
		if !v.sleep(backoff) {
			return
		}
		backoff = nextBackoff(backoff, v.vcfg.RestartMax)
	}
}

// pacerLoop emits exactly one frame per frame interval so Asterisk receives a
// continuous, evenly paced stream. Underruns are filled with silence.
func (v *virtual) pacerLoop(sink Sink) {
	defer v.wg.Done()
	ticker := time.NewTicker(v.frameDur)
	defer ticker.Stop()

	frame := make([]byte, v.frameBytes)
	for {
		select {
		case <-v.ctx.Done():
			return
		case <-ticker.C:
		}
		n := v.out.ReadAvailable(frame)
		if n == len(frame) {
			sink(frame)
			v.c.outFrames.Add(1)
			v.c.outBytes.Add(uint64(n))
			continue
		}
		if n == 0 && !v.vcfg.SilenceOnUnderrun {
			v.c.underruns.Add(1)
			continue
		}
		for i := n; i < len(frame); i++ {
			frame[i] = 0
		}
		sink(frame)
		v.c.outFrames.Add(1)
		v.c.outBytes.Add(uint64(len(frame)))
		v.c.underruns.Add(1)
	}
}

func (v *virtual) playbackArgs() []string {
	return []string{
		"--playback",
		"--device=" + v.vcfg.PlaybackSink,
		"--format=s16le",
		"--rate=" + strconv.Itoa(v.vcfg.DeviceRate),
		"--channels=" + strconv.Itoa(v.vcfg.DeviceChannels),
		"--latency-msec=" + strconv.FormatInt(v.vcfg.Latency.Milliseconds(), 10),
		"--raw",
	}
}

func (v *virtual) captureArgs() []string {
	return []string{
		"--device=" + v.vcfg.CaptureSource,
		"--format=s16le",
		"--rate=" + strconv.Itoa(v.vcfg.DeviceRate),
		"--channels=" + strconv.Itoa(v.vcfg.DeviceChannels),
		"--latency-msec=" + strconv.FormatInt(v.vcfg.Latency.Milliseconds(), 10),
		"--raw",
	}
}

func (v *virtual) Stats() Stats {
	st := v.c.snapshot("virtual")
	st.DroppedBytes = v.c.dropped.Load() + v.in.Dropped() + v.out.Dropped()
	resampling := "none"
	if !v.up.Passthrough() {
		resampling = fmt.Sprintf("%d->%d", v.cfg.CallRate, v.vcfg.DeviceRate)
	}
	st.Details = map[string]string{
		"playback_sink":   v.vcfg.PlaybackSink,
		"capture_source":  v.vcfg.CaptureSource,
		"device_rate":     strconv.Itoa(v.vcfg.DeviceRate),
		"device_channels": strconv.Itoa(v.vcfg.DeviceChannels),
		"resampling":      resampling,
		"in_buffer_bytes": strconv.Itoa(v.in.Capacity()),
		"out_buffer_bytes": strconv.Itoa(v.out.Capacity()),
	}
	return st
}

func (v *virtual) Close() error {
	v.mu.Lock()
	if v.closing {
		v.mu.Unlock()
		return nil
	}
	v.closing = true
	cmd := v.pacat
	v.mu.Unlock()

	if v.cancel != nil {
		v.cancel()
	}
	v.in.Close()
	v.out.Close()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}

	done := make(chan struct{})
	go func() {
		v.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		v.log.Warn("virtual audio shutdown timed out")
	}
	v.c.ready.Store(false)
	return nil
}

func (v *virtual) setPacat(cmd *exec.Cmd) {
	v.mu.Lock()
	v.pacat = cmd
	v.mu.Unlock()
}

func (v *virtual) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-v.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next <= 0 || next > max {
		return max
	}
	return next
}
