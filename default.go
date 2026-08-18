package beepout

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gen2brain/malgo"
)

// Package-level default speaker.
//
// The default speaker is a process-wide singleton used by the package-level
// Init, Close, Play, PlayAndWait, Suspend, Resume, Clear, Lock, and Unlock
// functions. It mirrors the API of github.com/gopxl/beep/v2/speaker (which
// is itself identical to github.com/gopxl/beep/speaker v1) so that code
// written against beep's package-level speaker API can be ported to beepout
// by changing only the import path.
//
// The default speaker is independent of any *Speaker created with New:
// calling New returns a fresh, self-managed *Speaker that does not
// interact with the default speaker. The two APIs coexist without
// interference.
//
// Locking model:
//
//   - defaultMu guards the defaultSpeaker pointer itself.
//   - defaultSpeaker.mu (the Speaker's internal mutex) guards the mixer
//     state and is the same mutex that the audio callback acquires. The
//     package-level Lock and Unlock functions acquire defaultSpeaker.mu
//     so that callers can safely mutate the state of streamers they
//     previously added via Play, exactly like beep v2's speaker.Lock.
//
// To avoid the "which speaker did I lock?" problem when Close replaces the
// default speaker, callers must follow the same contract as beep v2:
// do not call Init, Close, Play, PlayAndWait, Suspend, Resume, or Clear
// while holding the lock acquired via Lock. The intended pattern is to
// Lock, mutate streamer state directly, then Unlock.
var (
	defaultMu      sync.Mutex
	defaultSpeaker *Speaker
)

// InitOption is an optional configuration argument for the package-level
// Init function. An InitOption mutates an internal config builder; it is
// applied left-to-right, with later options overriding earlier ones for
// the same field.
//
// InitOption values are constructed by the WithBackends, WithVolume, and
// WithLogProc helper functions. Callers may also define their own
// InitOption values for use cases such as conditional configuration
// based on host platform detection.
//
// InitOption is beepout-specific: the beep v1/v2 speaker package has no
// options API because it does not expose backend configuration at all.
type InitOption func(*initConfig)

// initConfig is the internal builder that InitOption values mutate. It
// is not exported; callers interact with it only through InitOption
// values.
type initConfig struct {
	backends []malgo.Backend
	volume   float64
	logProc  malgo.LogProc
}

// WithBackends specifies the malgo backend priority list for the default
// Speaker. malgo tries each backend in order and uses the first one that
// initializes successfully on the host platform.
//
// If WithBackends is not passed to Init (or is passed with no
// arguments), Init uses malgo's platform default backend ordering.
//
// On Android, callers should pass:
//
//	beepout.Init(48000, 2048,
//	    beepout.WithBackends(malgo.BackendAaudio, malgo.BackendOpensl))
//
// so that AAudio is preferred (lower latency, better features) and
// OpenSL|ES is used as a fallback on older devices.
//
// WithBackends copies the caller's slice; external mutation of the
// argument after WithBackends returns does not affect the configured
// priority list.
//
// WithBackends is beepout-specific.
func WithBackends(backends ...malgo.Backend) InitOption {
	return func(c *initConfig) {
		if len(backends) == 0 {
			c.backends = nil
			return
		}
		cp := make([]malgo.Backend, len(backends))
		copy(cp, backends)
		c.backends = cp
	}
}

// WithVolume specifies the linear gain applied to every mixed sample
// before it is written to the device. 1.0 leaves the signal unchanged,
// 0.5 halves the amplitude, 0.0 is treated as the default (1.0) for
// Config-compatibility — callers that want true digital silence should
// pass a very small positive number such as 1e-10.
//
// If WithVolume is not passed to Init, the default Speaker uses a
// neutral volume of 1.0.
//
// WithVolume is beepout-specific.
func WithVolume(volume float64) InitOption {
	return func(c *initConfig) {
		c.volume = volume
	}
}

// WithLogProc specifies an optional malgo logging callback. malgo invokes
// the callback from arbitrary goroutines; callers are responsible for
// any synchronization required by their logging implementation.
//
// If WithLogProc is not passed to Init, no logging callback is
// registered.
//
// WithLogProc is beepout-specific.
func WithLogProc(logProc malgo.LogProc) InitOption {
	return func(c *initConfig) {
		c.logProc = logProc
	}
}

// Init initializes the package-level default Speaker with the given
// sample rate and buffer size, plus optional configuration applied via
// InitOption values (such as WithBackends, WithVolume, WithLogProc).
//
// The original two-argument form is preserved for backwards
// compatibility:
//
//	beepout.Init(48000, 2048)
//
// passes nil backends (malgo's platform default) and a neutral volume of
// 1.0. The variadic form allows additional configuration without
// requiring callers to construct a full Config:
//
//	beepout.Init(48000, 2048,
//	    beepout.WithBackends(malgo.BackendAaudio, malgo.BackendOpensl),
//	    beepout.WithVolume(0.8))
//
// Init must be called once before any other package-level function
// (other than Close) can be used. Init returns an error if the default
// Speaker has already been initialized; call Close first to
// reinitialize.
//
// sampleRate is in frames per second (e.g. 44100, 48000). bufferSize is
// the device period size in frames (e.g. 256, 2048); larger values
// reduce CPU usage, smaller values reduce latency.
//
// On failure, Init does not modify the default Speaker state: if a
// default Speaker was already initialized, it remains in place; if no
// default Speaker was initialized, it remains nil.
func Init(sampleRate int, bufferSize int, opts ...InitOption) error {
	cfg := initConfig{
		volume: 1.0, // default; WithVolume(0) is treated as 1.0 by New
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultSpeaker != nil {
		return errors.New("beepout: default speaker already initialized")
	}
	s, err := New(Config{
		SampleRate: sampleRate,
		BufferSize: bufferSize,
		Backends:   cfg.backends,
		Volume:     cfg.volume,
		LogProc:    cfg.logProc,
	})
	if err != nil {
		return err
	}
	defaultSpeaker = s
	return nil
}

// Close closes the package-level default Speaker and releases all of its
// resources. It is safe to call Close multiple times; subsequent calls are
// no-ops. After Close returns, the default Speaker must be reinitialized
// with Init before any other package-level function can be used again.
//
// Close does not return an error, matching the beep v1 and v2
// speaker.Close() signature. If you need the close error, use a *Speaker
// created with New and call its Close method directly.
func Close() {
	defaultMu.Lock()
	spk := defaultSpeaker
	if spk != nil {
		// Detach the default speaker before closing it so that
		// concurrent Init callers see a nil defaultSpeaker and can
		// create a fresh one without waiting for the (possibly
		// blocking) close to finish.
		defaultSpeaker = nil
	}
	defaultMu.Unlock()
	if spk != nil {
		_ = spk.Close()
	}
}

// Play adds the given streamers to the default Speaker's playback queue.
// Streamers are mixed additively; drained streamers are removed
// automatically by the audio callback.
//
// Play must not be called while the caller already holds the speaker lock
// acquired via Lock — that would deadlock. See Lock for details.
func Play(streamers ...Streamer) {
	defaultMu.Lock()
	spk := defaultSpeaker
	defaultMu.Unlock()
	if spk == nil {
		return
	}
	spk.Play(streamers...)
}

// PlayAndWait plays the given streamers through the default Speaker and
// blocks until all of them have drained. It then sleeps for the duration
// of one buffer period to allow any samples already in the device buffer
// to be played, mirroring the behavior of beep v1/v2 speaker.PlayAndWait.
//
// Like Play, PlayAndWait must not be called while the caller holds the
// speaker lock.
//
// Each streamer is wrapped internally so that its drain signal is
// observable from the calling goroutine. The wrapper satisfies
// beepout.Streamer (and therefore beep.Streamer from both v1 and v2)
// without altering the original streamer's samples or error.
func PlayAndWait(streamers ...Streamer) {
	defaultMu.Lock()
	spk := defaultSpeaker
	defaultMu.Unlock()
	if spk == nil {
		return
	}

	var wg sync.WaitGroup
	wg.Add(len(streamers))
	wrapped := make([]Streamer, len(streamers))
	for i, src := range streamers {
		wrapped[i] = newDoneStreamer(src, wg.Done)
	}

	// Add the wrapped streamers directly to the mixer under the speaker
	// lock. We bypass spk.Play so that the wait group is fully set up
	// before any wrapped streamer can drain (which would call wg.Done
	// from the audio callback's goroutine).
	spk.mu.Lock()
	var sampleRate, bufferSize int
	if !spk.closed {
		spk.mixer.add(wrapped...)
		sampleRate = spk.sampleRate
		bufferSize = spk.bufferSize
	}
	spk.mu.Unlock()

	wg.Wait()

	// Sleep for one buffer period so the device can flush any samples
	// that were already mixed when the last streamer drained.
	if sampleRate > 0 && bufferSize > 0 {
		time.Sleep(time.Second * time.Duration(bufferSize) / time.Duration(sampleRate))
	}
}

// Suspend suspends audio playback on the default Speaker by stopping the
// underlying malgo device. The device is not uninitialized; use Resume to
// restart it.
//
// Suspend is independent of the Speaker.Pause method, which uses a
// flag-based approach to silence the output without stopping the device.
// Suspend saves more power but has higher resume latency than Pause, and
// is intended for situations where no audio will be played for an
// extended period.
//
// Suspend must not be called while the caller holds the speaker lock
// acquired via Lock — that would deadlock. Suspend also must not run
// concurrently with Close on the default speaker; the two operations both
// touch the underlying malgo device and are not synchronized against
// each other (matching the same constraint in beep v1/v2's speaker
// package).
func Suspend() error {
	defaultMu.Lock()
	spk := defaultSpeaker
	defaultMu.Unlock()
	if spk == nil {
		return errors.New("beepout: default speaker not initialized")
	}

	spk.mu.Lock()
	closed := spk.closed
	spk.mu.Unlock()
	if closed {
		return errors.New("beepout: default speaker is closed")
	}
	return spk.suspend()
}

// Resume restarts the default Speaker's playback device after a prior
// Suspend. If the device was not suspended, Resume is a no-op.
//
// Resume is independent of the Speaker.Resume method, which undoes a
// Speaker.Pause call. The two address different layers: Speaker.Pause and
// Speaker.Resume toggle a flag in the mixer; Suspend and Resume toggle
// the device's start/stop state.
//
// Resume must not be called while the caller holds the speaker lock
// acquired via Lock — that would deadlock.
func Resume() error {
	defaultMu.Lock()
	spk := defaultSpeaker
	defaultMu.Unlock()
	if spk == nil {
		return errors.New("beepout: default speaker not initialized")
	}

	spk.mu.Lock()
	closed := spk.closed
	spk.mu.Unlock()
	if closed {
		return errors.New("beepout: default speaker is closed")
	}
	return spk.resume()
}

// Clear removes all queued streamers from the default Speaker. Samples
// already buffered by the device may still be played.
//
// Clear must not be called while the caller holds the speaker lock
// acquired via Lock — that would deadlock.
func Clear() {
	defaultMu.Lock()
	spk := defaultSpeaker
	defaultMu.Unlock()
	if spk == nil {
		return
	}
	spk.Clear()
}

// Lock acquires the default Speaker's mutex. While held, the audio
// callback will not advance any queued streamer, making it safe to mutate
// the state of a streamer that has already been added via Play.
//
// Lock must be paired with Unlock. Hold the lock for as short a time as
// possible — the audio callback blocks while the lock is held, and holding
// it for longer than one device period will cause an underrun.
//
// Do not call Play, PlayAndWait, Clear, Suspend, Resume, Init, or Close
// while holding the lock: those functions acquire the same mutex
// internally and would deadlock. The intended pattern is to Lock, mutate
// streamer state directly, then Unlock.
//
// Lock must only be called after Init has succeeded; calling Lock before
// Init is a programming error and Unlock will panic when it tries to
// release a mutex that was never acquired.
func Lock() {
	defaultMu.Lock()
	spk := defaultSpeaker
	defaultMu.Unlock()
	if spk != nil {
		spk.mu.Lock()
	}
}

// Unlock releases the default Speaker's mutex previously acquired by Lock.
//
// Unlock must only be called after a successful Lock. Calling Unlock
// without a prior Lock (or after the default Speaker has been reinitialized
// by Init between Lock and Unlock) is a programming error and may panic.
func Unlock() {
	defaultMu.Lock()
	spk := defaultSpeaker
	defaultMu.Unlock()
	if spk != nil {
		spk.mu.Unlock()
	}
}

// doneStreamer wraps a Streamer and invokes done exactly once, from any
// goroutine, when the underlying streamer reports it is drained (returns
// ok == false). It is used by PlayAndWait to know when each queued
// streamer has finished producing samples.
//
// doneStreamer satisfies the Streamer interface itself, so it can be added
// to the mixer in place of the original streamer. Its Stream and Err
// methods delegate to the wrapped streamer without altering the samples
// or error.
type doneStreamer struct {
	s    Streamer
	done func()
	once sync.Once
}

// newDoneStreamer returns a doneStreamer that calls done at most once when
// the wrapped streamer s is drained.
func newDoneStreamer(s Streamer, done func()) *doneStreamer {
	return &doneStreamer{s: s, done: done}
}

// Stream delegates to the wrapped streamer. If the wrapped streamer
// reports ok == false, the done callback is invoked exactly once via
// sync.Once, which is safe to call from any goroutine (including the
// audio callback's worker thread).
func (d *doneStreamer) Stream(samples [][2]float64) (int, bool) {
	n, ok := d.s.Stream(samples)
	if !ok {
		d.once.Do(d.done)
	}
	return n, ok
}

// Err returns whatever error the wrapped streamer reports.
func (d *doneStreamer) Err() error { return d.s.Err() }

// beepout-specific package-level functions for post-init configuration.
//
// The beep v1/v2 speaker API only allows the sample rate and buffer size
// to be configured at Init time; backends cannot be reconfigured without
// recompiling. beepout exposes SetBackends and Reinit so callers can
// change the malgo backend priority list (and other Config fields) at
// runtime, after the default Speaker has already been initialized.

// Backends returns the malgo backend priority list that the default
// Speaker was initialized with (via Init or Reinit). The returned slice
// is a fresh copy and may be safely modified by the caller.
//
// Returns nil if no default Speaker is currently initialized, or if the
// default Speaker was initialized via Init (which uses malgo's platform
// default ordering, represented internally as nil).
//
// Backends is beepout-specific: it has no equivalent in the beep v1/v2
// speaker API.
func Backends() []malgo.Backend {
	defaultMu.Lock()
	spk := defaultSpeaker
	defaultMu.Unlock()
	if spk == nil {
		return nil
	}
	return spk.Backends()
}

// SampleRate returns the playback sample rate in frames per second that
// the default Speaker was initialized with. Returns 0 if no default
// Speaker is currently initialized.
//
// SampleRate is beepout-specific: the beep v1/v2 speaker package does
// not expose a package-level SampleRate function. It is provided here
// for symmetry with the (*Speaker).SampleRate method.
func SampleRate() int {
	defaultMu.Lock()
	spk := defaultSpeaker
	defaultMu.Unlock()
	if spk == nil {
		return 0
	}
	return spk.SampleRate()
}

// BufferSize returns the device period size in frames that the default
// Speaker was initialized with. Returns 0 if no default Speaker is
// currently initialized.
//
// BufferSize is beepout-specific: the beep v1/v2 speaker package does
// not expose a package-level BufferSize function.
func BufferSize() int {
	defaultMu.Lock()
	spk := defaultSpeaker
	defaultMu.Unlock()
	if spk == nil {
		return 0
	}
	return spk.BufferSize()
}

// SetBackends re-initializes the default Speaker with the given malgo
// backend priority list, preserving the sample rate, buffer size, and
// volume that were configured by the previous Init or Reinit call.
//
// SetBackends is atomic with respect to failure: the new Speaker is
// constructed WITHOUT closing the current one. If the new Speaker
// cannot be created (for example because none of the requested backends
// are available on this platform), the current default Speaker is left
// untouched and SetBackends returns an error. Callers can rely on the
// default Speaker remaining usable after a failed SetBackends.
//
// On success, the old default Speaker is closed and replaced atomically:
// no callers observe a moment with no default Speaker. Any streamers
// that were queued via Play are lost when the old Speaker is closed;
// callers should re-add them after SetBackends returns successfully.
//
// SetBackends returns an error if no default Speaker has been
// initialized (call Init first) or if the new Speaker cannot be created.
//
// SetBackends must not be called concurrently with Init, Close, or
// another SetBackends/Reinit; lifecycle operations on the default
// Speaker are not serialized against each other.
//
// SetBackends is beepout-specific: it has no equivalent in the beep
// v1/v2 speaker API, which only allows backends to be configured at
// Init time.
func SetBackends(backends []malgo.Backend) error {
	defaultMu.Lock()
	spk := defaultSpeaker
	if spk == nil {
		defaultMu.Unlock()
		return errors.New("beepout: default speaker not initialized")
	}
	sampleRate := spk.sampleRate
	bufferSize := spk.bufferSize
	spk.mu.Lock()
	volume := spk.mixer.volume
	spk.mu.Unlock()
	defaultMu.Unlock()

	// Copy the caller's slice so external mutation does not affect the
	// new Speaker's recorded priority list.
	var backendsCopy []malgo.Backend
	if backends != nil {
		backendsCopy = make([]malgo.Backend, len(backends))
		copy(backendsCopy, backends)
	}

	// Try-before-buy: construct the new Speaker WITHOUT closing the old
	// one. If construction fails, the old Speaker remains the default
	// and the caller can recover without re-calling Init.
	newSpk, err := New(Config{
		SampleRate: sampleRate,
		BufferSize: bufferSize,
		Backends:   backendsCopy,
		Volume:     volume,
	})
	if err != nil {
		// Old speaker is still in place; nothing to restore.
		return fmt.Errorf("beepout: SetBackends: %w", err)
	}

	// Success: atomically swap default to new, then close old outside
	// the lock (Close can block waiting for the audio callback).
	defaultMu.Lock()
	defaultSpeaker = newSpk
	defaultMu.Unlock()
	_ = spk.Close()
	return nil
}

// Reinit re-initializes the default Speaker with the given Config,
// replacing the previous one entirely.
//
// Reinit is more flexible than SetBackends: it allows changing the
// sample rate, buffer size, volume, backend priority list, and log
// callback in a single call. Use SetBackends if only the backend
// priority list needs to change, or use the variadic Init with options
// (WithBackends, WithVolume, WithLogProc) for the initial configuration.
//
// Reinit is atomic with respect to failure: the new Speaker is
// constructed WITHOUT closing the current one. If the new Speaker
// cannot be created, the current default Speaker is left untouched
// and Reinit returns an error. Callers can rely on the default
// Speaker remaining usable after a failed Reinit.
//
// On success, the old default Speaker is closed and replaced
// atomically: no callers observe a moment with no default Speaker.
// Any streamers that were queued via Play are lost when the old
// Speaker is closed; callers should re-add them after Reinit returns
// successfully.
//
// Reinit returns an error if no default Speaker has been initialized
// (call Init first) or if the new Speaker cannot be created.
//
// Reinit must not be called concurrently with Init, Close, or another
// SetBackends/Reinit; lifecycle operations on the default Speaker are
// not serialized against each other.
//
// Reinit is beepout-specific: it has no equivalent in the beep v1/v2
// speaker API.
func Reinit(cfg Config) error {
	defaultMu.Lock()
	spk := defaultSpeaker
	if spk == nil {
		defaultMu.Unlock()
		return errors.New("beepout: default speaker not initialized")
	}
	defaultMu.Unlock()

	// Try-before-buy: construct the new Speaker WITHOUT closing the old
	// one. If construction fails, the old Speaker remains the default
	// and the caller can recover without re-calling Init.
	newSpk, err := New(cfg)
	if err != nil {
		return fmt.Errorf("beepout: Reinit: %w", err)
	}

	// Success: atomically swap default to new, then close old outside
	// the lock (Close can block waiting for the audio callback).
	defaultMu.Lock()
	defaultSpeaker = newSpk
	defaultMu.Unlock()
	_ = spk.Close()
	return nil
}
