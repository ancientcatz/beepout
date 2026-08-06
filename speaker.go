package beepout

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/gen2brain/malgo"
)

// channelCount is the number of output channels. beepout always outputs
// stereo audio to match the beep Streamer contract.
const channelCount = 2

// Config configures a Speaker at construction time. The zero value is not
// usable; SampleRate and BufferSize must be set explicitly.
type Config struct {
	// SampleRate is the playback sample rate in frames per second. Must be
	// positive. Typical values are 44100, 48000, etc.
	SampleRate int

	// BufferSize is the device period size in frames. Larger values reduce
	// CPU usage and the risk of underruns; smaller values reduce latency.
	// Must be positive. Common values range from 256 to 4096.
	BufferSize int

	// Backends is an ordered list of preferred malgo backends. The first
	// backend that initializes successfully is used. If empty, malgo's
	// platform default backend order applies.
	//
	// On Android, callers should pass:
	//
	//	[]malgo.Backend{malgo.BackendAaudio, malgo.BackendOpensl}
	//
	// so that AAudio is preferred (lower latency, better features) and
	// OpenSL ES is used as a fallback on older devices.
	Backends []malgo.Backend

	// Volume is a linear gain applied to every mixed sample before it is
	// written to the device. 1.0 leaves the signal unchanged, 0.0 mutes
	// playback, 0.5 halves the amplitude. The zero value defaults to 1.0
	// at construction time, so callers that want true silence must set
	// Volume explicitly to a very small positive number.
	Volume float64

	// LogProc, if non-nil, is invoked by malgo with diagnostic messages.
	// It may be called from any goroutine; callers are responsible for any
	// synchronization required by their logging implementation.
	LogProc malgo.LogProc
}

// Speaker is a malgo-backed audio sink that plays Streamer values. A Speaker
// owns a malgo context and a single playback device. Speaker methods are
// safe to call from multiple goroutines concurrently.
//
// After Close returns, the Speaker is no longer usable. All public methods
// become no-ops (Close remains safe to call any number of times).
type Speaker struct {
	mu sync.Mutex

	ctx        *malgo.AllocatedContext
	dev        *malgo.Device
	mixer      *mixer
	sampleRate int

	// buf is a scratch buffer reused by the audio callback to mix into.
	// It is only touched while mu is held, and the callback is serialized
	// by malgo's worker thread, so it is safe to read after unlocking.
	buf [][2]float64

	// tmp is the per-call scratch buffer passed to mixer.mix. Kept on the
	// struct to avoid stack growth and per-call allocation.
	tmp [512][2]float64

	closed    bool
	closeOnce sync.Once
	closeErr  error
}

// New creates and starts a Speaker.
//
// New initializes a malgo context using the configured backend priority,
// creates a stereo Float32 playback device with the requested sample rate
// and period size, and starts the device automatically. The returned Speaker
// is ready to accept streamers via Play.
//
// If New returns an error, no resources are leaked: any partially
// initialized malgo state is cleaned up before returning.
func New(cfg Config) (*Speaker, error) {
	if cfg.SampleRate <= 0 {
		return nil, errors.New("beepout: Config.SampleRate must be positive")
	}
	if cfg.BufferSize <= 0 {
		return nil, errors.New("beepout: Config.BufferSize must be positive")
	}

	// A zero Volume is treated as the neutral default. Callers who want
	// silence must set Volume explicitly to a positive value smaller than
	// the smallest representable gain they care about.
	volume := cfg.Volume
	if volume == 0 {
		volume = 1.0
	}

	// Allocate the speaker first so the close path has somewhere to record
	// state if initialization fails partway through.
	s := &Speaker{
		sampleRate: cfg.SampleRate,
		mixer:      newMixer(volume),
		buf:        make([][2]float64, cfg.BufferSize),
	}

	// Initialize the malgo context with the caller's backend priority.
	// malgo tries each backend in order and returns the first that works.
	ctx, err := malgo.InitContext(cfg.Backends, malgo.ContextConfig{}, cfg.LogProc)
	if err != nil {
		return nil, fmt.Errorf("beepout: init malgo context: %w", err)
	}
	s.ctx = ctx

	// Configure a stereo Float32 LE playback device.
	deviceConfig := malgo.DefaultDeviceConfig(malgo.Playback)
	deviceConfig.SampleRate = uint32(cfg.SampleRate)
	deviceConfig.PeriodSizeInFrames = uint32(cfg.BufferSize)
	deviceConfig.Playback.Format = malgo.FormatF32
	deviceConfig.Playback.Channels = channelCount

	// The data callback captures s; it is invoked from malgo's worker
	// thread for as long as the device is started.
	onData := func(pOutput, _ []byte, frameCount uint32) {
		s.onAudio(pOutput, frameCount)
	}

	dev, err := malgo.InitDevice(ctx.Context, deviceConfig, malgo.DeviceCallbacks{Data: onData})
	if err != nil {
		_ = ctx.Uninit()
		ctx.Free()
		return nil, fmt.Errorf("beepout: init malgo device: %w", err)
	}
	s.dev = dev

	if err := dev.Start(); err != nil {
		dev.Uninit()
		_ = ctx.Uninit()
		ctx.Free()
		return nil, fmt.Errorf("beepout: start malgo device: %w", err)
	}

	return s, nil
}

// onAudio is the malgo data callback. It is invoked from malgo's worker
// thread and must not block on anything other than the Speaker mutex.
//
// The callback mixes queued streamers into s.buf while holding s.mu, then
// converts the float64 samples to interleaved float32 outside the lock and
// writes them to pOutput. If the speaker is closed or paused, pOutput is
// filled with silence instead.
func (s *Speaker) onAudio(pOutput []byte, frameCount uint32) {
	n := int(frameCount)
	if n == 0 || len(pOutput) == 0 {
		return
	}

	// s.buf is owned by the callback while mu is held; the conversion to
	// float32 happens after unlocking, which is safe because the callback
	// is serialized by malgo's worker thread — the next invocation cannot
	// start until this one returns.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		zeroFloat32(pOutput)
		return
	}

	if cap(s.buf) < n {
		s.buf = make([][2]float64, n)
	} else {
		s.buf = s.buf[:n]
	}
	for i := range s.buf {
		s.buf[i] = [2]float64{}
	}
	s.mixer.mix(s.buf, &s.tmp)
	s.mu.Unlock()

	writeFloat32Output(pOutput, s.buf)
}

// writeFloat32Output converts the stereo float64 frames in buf to
// interleaved stereo float32 samples written into out, clamping each sample
// to [-1, 1]. It performs no allocations.
func writeFloat32Output(out []byte, buf [][2]float64) {
	if len(buf) == 0 || len(out) < 8*len(buf) {
		return
	}
	// Reinterpret out as a []float32 of length 2*len(buf). out's length is
	// guaranteed by malgo to be exactly frameCount * channels *
	// bytesPerSample (== n * 2 * 4), so the slice is well-sized.
	f32 := unsafe.Slice((*float32)(unsafe.Pointer(&out[0])), len(buf)*2)
	for i := range buf {
		l := buf[i][0]
		r := buf[i][1]
		if l > 1 {
			l = 1
		} else if l < -1 {
			l = -1
		}
		if r > 1 {
			r = 1
		} else if r < -1 {
			r = -1
		}
		f32[i*2+0] = float32(l)
		f32[i*2+1] = float32(r)
	}
}

// zeroFloat32 fills out (which must be a multiple of 4 bytes long) with
// zeros, producing digital silence for a Float32 device.
func zeroFloat32(out []byte) {
	for i := range out {
		out[i] = 0
	}
}

// Play adds the given streamers to the playback queue. Streamers are mixed
// together additively. Drained streamers are removed from the queue
// automatically by the audio callback.
//
// Play is safe to call from multiple goroutines, but must not be called
// while the caller already holds the speaker lock acquired via Lock — that
// would deadlock. See Lock for details.
func (s *Speaker) Play(streamers ...Streamer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.mixer.add(streamers...)
}

// Clear removes all queued streamers. Samples already buffered by the
// device may still be played.
func (s *Speaker) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.mixer.clear()
}

// Pause stops emitting audio from queued streamers without stopping the
// device. While paused, the audio callback outputs silence and queued
// streamers are not advanced, so playback resumes from the same position.
//
// Pause is independent from malgo's device start/stop state; the device
// keeps running so that Resume can take effect with the lowest possible
// latency. To save power when no audio is playing for an extended period,
// consider calling Close instead.
func (s *Speaker) Pause() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.mixer.paused = true
}

// Resume undoes a prior Pause. If the speaker was not paused, Resume is a
// no-op.
func (s *Speaker) Resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.mixer.paused = false
}

// Lock acquires the speaker mutex. While held, the audio callback will not
// advance any queued streamer, which makes it safe to mutate the state of a
// streamer that has already been added via Play.
//
// Lock must be paired with Unlock. Hold the lock for as short a time as
// possible — the audio callback blocks while the lock is held, and holding
// it for longer than one device period will cause an underrun.
//
// Do not call Play, Clear, Pause, Resume, or Close while holding the lock:
// those methods acquire the same mutex internally and would deadlock. The
// intended pattern is to Lock, mutate streamer state directly, then Unlock.
func (s *Speaker) Lock() { s.mu.Lock() }

// Unlock releases the speaker mutex previously acquired by Lock.
func (s *Speaker) Unlock() { s.mu.Unlock() }

// SampleRate returns the playback sample rate in frames per second. The
// value is taken from Config.SampleRate at construction time and does not
// change for the lifetime of the Speaker.
func (s *Speaker) SampleRate() int { return s.sampleRate }

// Close stops the playback device, uninitializes it, uninitializes the
// malgo context, and frees all associated resources.
//
// Close is idempotent: calling it multiple times is safe and returns the
// error from the first invocation on every subsequent call.
//
// After Close returns, every other Speaker method becomes a no-op.
func (s *Speaker) Close() error {
	s.closeOnce.Do(func() {
		// Mark the speaker closed under the lock so the audio callback
		// stops touching the mixer. We then release the lock before
		// calling into malgo: device shutdown can block waiting for the
		// worker thread, and we must not hold our mutex across that.
		s.mu.Lock()
		s.closed = true
		s.mixer.clear()
		s.mu.Unlock()

		if s.dev != nil {
			if err := s.dev.Stop(); err != nil && s.closeErr == nil {
				s.closeErr = fmt.Errorf("beepout: stop device: %w", err)
			}
			s.dev.Uninit()
			s.dev = nil
		}
		if s.ctx != nil {
			if err := s.ctx.Uninit(); err != nil && s.closeErr == nil {
				s.closeErr = fmt.Errorf("beepout: uninit context: %w", err)
			}
			s.ctx.Free()
			s.ctx = nil
		}
	})
	return s.closeErr
}
