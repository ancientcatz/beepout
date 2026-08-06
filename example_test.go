package beepout

import (
	"math"
	"testing"
	"time"
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
