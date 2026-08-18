package beepout

import (
	"math"
	"testing"
	"time"

	"github.com/gen2brain/malgo"
)

// Example_sineWave generates a 440 Hz sine tone using beepout's own
// StreamerFunc type and plays it through the default backend. It does not
// depend on beep at all, demonstrating that the beepout API is usable
// standalone.
func Example_sineWave() {
	const sampleRate = 44100
	const freq = 440.0

	s, err := New(Config{
		SampleRate: sampleRate,
		BufferSize: 2048,
		Volume:     0.5,
	})
	if err != nil {
		panic(err)
	}
	defer s.Close()

	// A self-contained infinite sine generator. The phase is tracked in
	// the closure and wrapped with math.Modf so it stays in [0, 1).
	dt := freq / float64(sampleRate)
	var t float64
	tone := StreamerFunc(func(samples [][2]float64) (int, bool) {
		for i := range samples {
			v := math.Sin(t * 2.0 * math.Pi)
			samples[i][0] = v
			samples[i][1] = v
			_, t = math.Modf(t + dt)
		}
		return len(samples), true
	})

	s.Play(tone)
	<-time.After(2 * time.Second)
	s.Clear()
}

// TestStreamerFuncSatisfiesInterface confirms at compile time that
// beepout.StreamerFunc implements beepout.Streamer. If the interface
// drifts, this test will fail to compile.
func TestStreamerFuncSatisfiesInterface(t *testing.T) {
	var _ Streamer = StreamerFunc(nil)
}

// TestStreamerFuncStream verifies that StreamerFunc.Stream delegates to the
// wrapped function and reports the returned values verbatim.
func TestStreamerFuncStream(t *testing.T) {
	calls := 0
	sf := StreamerFunc(func(samples [][2]float64) (int, bool) {
		calls++
		for i := range samples {
			samples[i] = [2]float64{float64(i), -float64(i)}
		}
		return len(samples), true
	})

	buf := make([][2]float64, 4)
	n, ok := sf.Stream(buf)
	if !ok {
		t.Fatalf("Stream returned ok=false; want true")
	}
	if n != len(buf) {
		t.Fatalf("Stream returned n=%d; want %d", n, len(buf))
	}
	if calls != 1 {
		t.Fatalf("wrapped function called %d times; want 1", calls)
	}
	for i := range buf {
		if buf[i] != [2]float64{float64(i), -float64(i)} {
			t.Fatalf("buf[%d] = %v; want %v", i, buf[i], [2]float64{float64(i), -float64(i)})
		}
	}
	if err := sf.Err(); err != nil {
		t.Fatalf("Err() = %v; want nil", err)
	}
}

// TestMixerDrainedStreamerRemoval exercises the core mixer loop without
// requiring any audio hardware. It feeds the mixer a streamer that produces
// exactly four frames before draining, then verifies that:
//
//   - the mixer removes the drained streamer from its internal slice,
//   - subsequent frames within the same call are silence,
//   - the produced samples match what the streamer returned, scaled by the
//     configured volume.
func TestMixerDrainedStreamerRemoval(t *testing.T) {
	m := newMixer(0.5)

	// A finite streamer that yields 4 frames total, in two calls of 2.
	produced := 0
	src := StreamerFunc(func(samples [][2]float64) (int, bool) {
		if produced >= 4 {
			return 0, false
		}
		canProduce := min(4-produced, len(samples))
		for i := range canProduce {
			samples[i] = [2]float64{1.0, 1.0}
		}
		produced += canProduce
		return canProduce, produced < 4
	})

	m.add(src)

	var tmp [512][2]float64
	out := make([][2]float64, 8)
	for i := range out {
		out[i] = [2]float64{-99, -99} // poison
	}
	m.mix(out, &tmp)

	// First four frames should be 0.5 (1.0 * volume), the remaining four
	// should be silence (zero).
	for i := range 4 {
		want := [2]float64{0.5, 0.5}
		if out[i] != want {
			t.Fatalf("out[%d] = %v; want %v", i, out[i], want)
		}
	}
	for i := 4; i < 8; i++ {
		if out[i] != [2]float64{0, 0} {
			t.Fatalf("out[%d] = %v; want silence", i, out[i])
		}
	}
	if m.len() != 0 {
		t.Fatalf("mixer len after drain = %d; want 0", m.len())
	}
}

// TestMixerPausedOutputsSilence verifies that a paused mixer leaves the
// output buffer untouched, so callers see the silence they pre-zeroed.
func TestMixerPausedOutputsSilence(t *testing.T) {
	m := newMixer(1.0)
	m.add(StreamerFunc(func(samples [][2]float64) (int, bool) {
		for i := range samples {
			samples[i] = [2]float64{1.0, 1.0}
		}
		return len(samples), true
	}))
	m.paused = true

	var tmp [512][2]float64
	out := make([][2]float64, 4)
	for i := range out {
		out[i] = [2]float64{}
	}
	m.mix(out, &tmp)

	for i := range out {
		if out[i] != [2]float64{0, 0} {
			t.Fatalf("paused mixer wrote %v at out[%d]; want silence", out[i], i)
		}
	}
	if m.len() != 1 {
		t.Fatalf("paused mixer mutated its streamer list (len=%d); want 1", m.len())
	}
}

// TestMixerClearReleasesStreamers checks that Clear empties the queue and
// drops references to the previously queued streamers.
func TestMixerClearReleasesStreamers(t *testing.T) {
	m := newMixer(1.0)
	m.add(
		StreamerFunc(func(_ [][2]float64) (int, bool) { return 0, false }),
		StreamerFunc(func(_ [][2]float64) (int, bool) { return 0, false }),
	)
	if m.len() != 2 {
		t.Fatalf("len after add = %d; want 2", m.len())
	}
	m.clear()
	if m.len() != 0 {
		t.Fatalf("len after clear = %d; want 0", m.len())
	}
}

// TestMixerMultipleStreamersMixesAdditively confirms that two concurrent
// streamers are summed into the output frame-by-frame.
func TestMixerMultipleStreamersMixesAdditively(t *testing.T) {
	m := newMixer(1.0)
	m.add(StreamerFunc(func(samples [][2]float64) (int, bool) {
		for i := range samples {
			samples[i] = [2]float64{0.25, 0.25}
		}
		return len(samples), true
	}))
	m.add(StreamerFunc(func(samples [][2]float64) (int, bool) {
		for i := range samples {
			samples[i] = [2]float64{0.75, 0.75}
		}
		return len(samples), true
	}))

	var tmp [512][2]float64
	out := make([][2]float64, 4)
	for i := range out {
		out[i] = [2]float64{}
	}
	m.mix(out, &tmp)

	for i := range out {
		want := [2]float64{1.0, 1.0}
		if out[i] != want {
			t.Fatalf("out[%d] = %v; want %v", i, out[i], want)
		}
	}
	if m.len() != 2 {
		t.Fatalf("len after mix = %d; want 2 (neither streamer should drain)", m.len())
	}
}

// TestMixerLargeOutputUsesMultipleChunks makes sure the 512-frame tmp
// chunking loop works correctly when the requested output exceeds the
// scratch buffer size.
func TestMixerLargeOutputUsesMultipleChunks(t *testing.T) {
	m := newMixer(1.0)
	m.add(StreamerFunc(func(samples [][2]float64) (int, bool) {
		for i := range samples {
			samples[i] = [2]float64{1.0, -1.0}
		}
		return len(samples), true
	}))

	var tmp [512][2]float64
	out := make([][2]float64, 1200) // larger than tmp
	for i := range out {
		out[i] = [2]float64{}
	}
	m.mix(out, &tmp)

	for i := range out {
		if out[i] != [2]float64{1.0, -1.0} {
			t.Fatalf("out[%d] = %v; want {1,-1}", i, out[i])
		}
	}
}

// TestDoneStreamerInvokesDoneExactlyOnce verifies that doneStreamer calls
// the done callback exactly one time when the wrapped streamer drains,
// even if Stream is called multiple times after drainage.
func TestDoneStreamerInvokesDoneExactlyOnce(t *testing.T) {
	calls := 0
	src := StreamerFunc(func(_ [][2]float64) (int, bool) {
		return 0, false // already drained
	})
	d := newDoneStreamer(src, func() { calls++ })

	buf := make([][2]float64, 4)
	for range 3 {
		d.Stream(buf)
	}
	if calls != 1 {
		t.Fatalf("done called %d times; want 1", calls)
	}
	if err := d.Err(); err != nil {
		t.Fatalf("Err() = %v; want nil", err)
	}
}

// TestDoneStreamerDelegatesSamples confirms that doneStreamer passes
// through the sample values and length returned by the wrapped streamer
// without alteration.
func TestDoneStreamerDelegatesSamples(t *testing.T) {
	src := StreamerFunc(func(samples [][2]float64) (int, bool) {
		for i := range samples {
			samples[i] = [2]float64{float64(i), -float64(i)}
		}
		return len(samples), true
	})
	d := newDoneStreamer(src, func() { /* never called */ })

	buf := make([][2]float64, 4)
	n, ok := d.Stream(buf)
	if !ok || n != len(buf) {
		t.Fatalf("Stream returned n=%d ok=%v; want %d true", n, ok, len(buf))
	}
	for i := range buf {
		if buf[i] != [2]float64{float64(i), -float64(i)} {
			t.Fatalf("buf[%d] = %v; want %v", i, buf[i], [2]float64{float64(i), -float64(i)})
		}
	}
}

// TestDoneStreamerSatisfiesStreamer is a compile-time assertion that
// doneStreamer satisfies the Streamer interface.
func TestDoneStreamerSatisfiesStreamer(t *testing.T) {
	var _ Streamer = (*doneStreamer)(nil)
}

// TestInitRejectsDoubleInit verifies that calling Init twice returns an
// error on the second call. We don't actually want to construct a real
// Speaker here (no audio device in CI), so this test is skipped if the
// first Init fails. When it can run, it asserts that the second Init
// fails with the expected error.
func TestInitRejectsDoubleInit(t *testing.T) {
	// Reset the default speaker from any prior test.
	Close()

	if err := Init(48000, 2048); err != nil {
		// No audio device available in the test environment; skip the
		// rest of the test rather than failing it.
		t.Skipf("Init failed (likely no audio device in CI): %v", err)
	}
	defer Close()

	if err := Init(48000, 2048); err == nil {
		t.Fatalf("second Init returned nil error; want non-nil")
	}
}

// TestPlayWithoutInitIsNoop verifies that calling Play before Init does
// not panic and is effectively a no-op.
func TestPlayWithoutInitIsNoop(t *testing.T) {
	Close() // ensure no default speaker

	// Should not panic.
	Play(StreamerFunc(func(_ [][2]float64) (int, bool) { return 0, false }))
	Clear()
	if err := Suspend(); err == nil {
		t.Fatalf("Suspend before Init returned nil; want error")
	}
	if err := Resume(); err == nil {
		t.Fatalf("Resume before Init returned nil; want error")
	}
}

// TestBackendsReturnsNilWhenUninitialized verifies that Backends returns
// nil when no default Speaker has been initialized.
func TestBackendsReturnsNilWhenUninitialized(t *testing.T) {
	Close() // ensure no default speaker
	if got := Backends(); got != nil {
		t.Fatalf("Backends() before Init = %v; want nil", got)
	}
}

// TestSampleRateReturnsZeroWhenUninitialized verifies that SampleRate
// returns 0 when no default Speaker has been initialized.
func TestSampleRateReturnsZeroWhenUninitialized(t *testing.T) {
	Close()
	if got := SampleRate(); got != 0 {
		t.Fatalf("SampleRate() before Init = %d; want 0", got)
	}
}

// TestBufferSizeReturnsZeroWhenUninitialized verifies that BufferSize
// returns 0 when no default Speaker has been initialized.
func TestBufferSizeReturnsZeroWhenUninitialized(t *testing.T) {
	Close()
	if got := BufferSize(); got != 0 {
		t.Fatalf("BufferSize() before Init = %d; want 0", got)
	}
}

// TestSetBackendsRequiresInit verifies that SetBackends returns an
// error when no default Speaker has been initialized.
func TestSetBackendsRequiresInit(t *testing.T) {
	Close()
	if err := SetBackends(nil); err == nil {
		t.Fatalf("SetBackends before Init returned nil; want error")
	}
}

// TestReinitRequiresInit verifies that Reinit returns an error when no
// default Speaker has been initialized.
func TestReinitRequiresInit(t *testing.T) {
	Close()
	if err := Reinit(Config{SampleRate: 48000, BufferSize: 2048}); err == nil {
		t.Fatalf("Reinit before Init returned nil; want error")
	}
}

// TestInstanceBackendsReturnsConfiguredBackends exercises the
// (*Speaker).Backends instance method on a Speaker created via New.
// It uses the Null backend so the test can run on headless CI.
//
// If the Null backend is not available on this platform, the test is
// skipped rather than failed.
func TestInstanceBackendsReturnsConfiguredBackends(t *testing.T) {
	want := []malgo.Backend{malgo.BackendNull}

	s, err := New(Config{
		SampleRate: 48000,
		BufferSize: 256,
		Backends:   want,
		Volume:     1.0,
	})
	if err != nil {
		t.Skipf("New with BackendNull failed (likely no Null backend on this platform): %v", err)
	}
	defer s.Close()

	got := s.Backends()
	if len(got) != len(want) {
		t.Fatalf("Backends() returned %d entries; want %d", len(got), len(want))
	}
	for i, b := range want {
		if got[i] != b {
			t.Fatalf("Backends()[%d] = %v; want %v", i, got[i], b)
		}
	}

	// Verify the returned slice is a copy, not the internal slice.
	got[0] = malgo.BackendAaudio // mutate the copy
	again := s.Backends()
	if again[0] != malgo.BackendNull {
		t.Fatalf("Backends() returned %v after mutating previous copy; want %v",
			again[0], malgo.BackendNull)
	}
}

// TestInstanceBackendsNilWhenConfiguredWithNil verifies that a Speaker
// constructed with Config.Backends == nil (the platform default)
// reports nil from Backends.
func TestInstanceBackendsNilWhenConfiguredWithNil(t *testing.T) {
	// Construct with no backends; we expect either an error (no
	// platform default backend available) or a Speaker whose Backends
	// returns nil.
	s, err := New(Config{
		SampleRate: 48000,
		BufferSize: 256,
		// Backends intentionally nil
		Volume: 1.0,
	})
	if err != nil {
		t.Skipf("New with nil Backends failed (likely no audio device in CI): %v", err)
	}
	defer s.Close()

	if got := s.Backends(); got != nil {
		t.Fatalf("Backends() = %v; want nil for nil-configured Speaker", got)
	}
}

// TestInstanceBufferSize verifies the (*Speaker).BufferSize instance
// method returns the value configured at construction time.
func TestInstanceBufferSize(t *testing.T) {
	s, err := New(Config{
		SampleRate: 48000,
		BufferSize: 256,
		Backends:   []malgo.Backend{malgo.BackendNull},
		Volume:     1.0,
	})
	if err != nil {
		t.Skipf("New with BackendNull failed: %v", err)
	}
	defer s.Close()

	if got := s.BufferSize(); got != 256 {
		t.Fatalf("BufferSize() = %d; want 256", got)
	}
	if got := s.SampleRate(); got != 48000 {
		t.Fatalf("SampleRate() = %d; want 48000", got)
	}
}

// TestWithBackendsCopiesSlice verifies that WithBackends copies the
// caller's slice, so external mutation after the option returns does
// not affect the configured priority list.
func TestWithBackendsCopiesSlice(t *testing.T) {
	src := []malgo.Backend{malgo.BackendAaudio, malgo.BackendOpensl}
	opt := WithBackends(src...)

	cfg := initConfig{}
	opt(&cfg)

	// Mutate the original slice; the captured copy must not change.
	src[0] = malgo.BackendWasapi
	src[1] = malgo.BackendCoreaudio

	if cfg.backends[0] != malgo.BackendAaudio {
		t.Fatalf("cfg.backends[0] = %v; want %v (WithBackends did not copy)", cfg.backends[0], malgo.BackendAaudio)
	}
	if cfg.backends[1] != malgo.BackendOpensl {
		t.Fatalf("cfg.backends[1] = %v; want %v (WithBackends did not copy)", cfg.backends[1], malgo.BackendOpensl)
	}
}

// TestWithBackendsEmptyArgsReturnsNil verifies that WithBackends with
// no arguments results in a nil backends field (the platform default).
func TestWithBackendsEmptyArgsReturnsNil(t *testing.T) {
	opt := WithBackends()
	cfg := initConfig{backends: []malgo.Backend{malgo.BackendAaudio}} // pre-set
	opt(&cfg)
	if cfg.backends != nil {
		t.Fatalf("cfg.backends = %v; want nil after WithBackends()", cfg.backends)
	}
}

// TestWithVolumeSetsField verifies that WithVolume mutates the volume
// field of the init config.
func TestWithVolumeSetsField(t *testing.T) {
	cfg := initConfig{}
	WithVolume(0.7)(&cfg)
	if cfg.volume != 0.7 {
		t.Fatalf("cfg.volume = %v; want 0.7", cfg.volume)
	}
}

// TestInitVariadicTwoArgFormPreservesOldBehavior is a compile-time
// assertion that the original two-argument form of Init still
// compiles. It does not actually call Init (no audio device in CI).
func TestInitVariadicTwoArgFormPreservesOldBehavior(t *testing.T) {
	// Verify the signature accepts exactly two int arguments.
	var twoArgForm func(int, int) error
	// Init has signature func(int, int, ...InitOption) error, which
	// satisfies the two-arg call pattern but does NOT satisfy the
	// two-arg func type (because of the variadic). So we check the
	// call form directly via a closure:
	var _ = func() {
		_ = Init(48000, 2048)
	}
	_ = twoArgForm // silence unused warning
}

// TestInitVariadicWithOptionsAcceptsOptions is a compile-time assertion
// that the variadic form of Init accepts InitOption values.
func TestInitVariadicWithOptionsAcceptsOptions(t *testing.T) {
	var _ = func() error {
		return Init(48000, 2048,
			WithBackends(malgo.BackendAaudio, malgo.BackendOpensl),
			WithVolume(0.8),
			WithLogProc(func(msg string) {}),
		)
	}
}

// TestSetBackendsFailureLeavesOldSpeakerInitialized verifies that when
// SetBackends fails to create a new Speaker (because the requested
// backend is not available), the previously initialized default
// Speaker remains in place — i.e. the rollback contract holds.
//
// This test requires the default Speaker to be initialized first,
// which requires an audio device. CI environments without an audio
// device skip this test rather than fail it.
func TestSetBackendsFailureLeavesOldSpeakerInitialized(t *testing.T) {
	Close() // ensure no default speaker

	// Initialize with the platform default. Skip if no audio device.
	if err := Init(48000, 2048); err != nil {
		t.Skipf("Init failed (likely no audio device in CI): %v", err)
	}
	defer Close()

	// Capture the configured sample rate before the failed SetBackends.
	// We can't directly inspect the default Speaker pointer, but we
	// can compare the public SampleRate() before and after.
	srBefore := SampleRate()
	bsBefore := BufferSize()

	// Try to switch to a backend that's almost certainly not available
	// on the current host (use an obscure combination). If even one
	// happens to be available, the test still passes because the new
	// Speaker would just be a successful replacement.
	_ = SetBackends([]malgo.Backend{malgo.BackendJack, malgo.BackendSndio})

	srAfter := SampleRate()
	bsAfter := BufferSize()

	// Whether SetBackends succeeded or failed, the public state must
	// remain consistent: a successful SetBackends preserves sampleRate
	// and bufferSize by design, and a failed SetBackends leaves them
	// untouched by the rollback contract.
	if srBefore != srAfter {
		t.Fatalf("SampleRate changed from %d to %d across SetBackends (rollback broken)",
			srBefore, srAfter)
	}
	if bsBefore != bsAfter {
		t.Fatalf("BufferSize changed from %d to %d across SetBackends (rollback broken)",
			bsBefore, bsAfter)
	}
}

// TestReinitFailureLeavesOldSpeakerInitialized verifies that when
// Reinit fails to create a new Speaker, the previously initialized
// default Speaker remains in place — i.e. the rollback contract holds.
//
// Same CI skip contract as TestSetBackendsFailureLeavesOldSpeakerInitialized.
func TestReinitFailureLeavesOldSpeakerInitialized(t *testing.T) {
	Close()
	if err := Init(48000, 2048); err != nil {
		t.Skipf("Init failed (likely no audio device in CI): %v", err)
	}
	defer Close()

	srBefore := SampleRate()

	// Try to reinit with an invalid config (zero SampleRate). This
	// must fail without touching the existing default Speaker.
	if err := Reinit(Config{SampleRate: 0, BufferSize: 2048}); err == nil {
		t.Fatalf("Reinit with zero SampleRate returned nil; want error")
	}

	if srAfter := SampleRate(); srAfter != srBefore {
		t.Fatalf("SampleRate changed from %d to %d after failed Reinit (rollback broken)",
			srBefore, srAfter)
	}
}
