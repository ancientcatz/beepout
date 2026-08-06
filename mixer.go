// Package beepout implements playback of audio streamers through the
// malgo (miniaudio) backend.
//
// beepout is structurally compatible with both github.com/gopxl/beep and
// github.com/gopxl/beep/v2: it accepts any value whose Stream and Err methods
// match the well-known beep.Streamer signature, without importing either
// beep module from the library implementation. See the Streamer interface in
// this file for the exact contract.
package beepout

// Streamer is the minimal contract required by beepout for an audio
// source.
//
// The signature is identical to the Streamer interface exposed by both
// github.com/gopxl/beep and github.com/gopxl/beep/v2, so values of type
// beep.Streamer from either version are accepted transparently by Play and
// any other beepout API that takes a Streamer. Future beep releases that
// preserve this signature will continue to work without code changes.
//
// Stream copies at most len(samples) next stereo frames into samples. Each
// frame is a [2]float64 where index 0 is the left channel and index 1 is the
// right channel. Sample values are linear PCM in the range [-1, 1].
//
// Stream returns the number of frames streamed and a boolean ok that reports
// whether more samples may follow. The three valid return patterns are:
//
//  1. n == len(samples) && ok            — the streamer produced all requested frames.
//  2. 0 < n && n < len(samples) && ok    — the streamer is now drained; only case 3 may follow.
//  3. n == 0 && !ok                      — the streamer is exhausted permanently.
//
// Err returns a non-nil error if streaming failed. When Err returns a non-nil
// error, Stream must return 0, false forever afterwards.
type Streamer interface {
	Stream(samples [][2]float64) (n int, ok bool)
	Err() error
}

// StreamerFunc adapts an ordinary function into a Streamer. It is the
// structural twin of beep.StreamerFunc from both beep v1 and v2, so a value
// of type beep.StreamerFunc satisfies beepout.Streamer directly.
//
// Example:
//
//	tone := beepout.StreamerFunc(func(samples [][2]float64) (int, bool) {
//	    for i := range samples {
//	        samples[i] = [2]float64{0.5, 0.5}
//	    }
//	    return len(samples), true
//	})
//	speaker.Play(tone)
type StreamerFunc func(samples [][2]float64) (n int, ok bool)

// Stream calls the wrapped function.
func (sf StreamerFunc) Stream(samples [][2]float64) (n int, ok bool) {
	return sf(samples)
}

// Err always returns nil. StreamerFunc has no error state.
func (sf StreamerFunc) Err() error { return nil }

// mixer is an internal N-channel stereo mixer. It mixes any number of
// streamers into a single stereo stream, removes drained streamers
// automatically, applies a volume multiplier, and produces silence when
// empty or paused.
//
// All methods assume the caller holds the owning Speaker's mutex; the mixer
// does no locking of its own. This keeps the audio-callback path lock-free
// beyond the single Speaker mutex.
type mixer struct {
	streamers []Streamer
	paused    bool
	volume    float64
}

// newMixer returns a mixer with the given initial volume.
func newMixer(volume float64) *mixer {
	return &mixer{volume: volume}
}

// add appends streamers to the queue.
func (m *mixer) add(streamers ...Streamer) {
	m.streamers = append(m.streamers, streamers...)
}

// clear removes all queued streamers and zeroes the backing slice so the
// streamers can be garbage collected.
func (m *mixer) clear() {
	for i := range m.streamers {
		m.streamers[i] = nil
	}
	m.streamers = m.streamers[:0]
}

// len returns the number of streamers currently queued.
func (m *mixer) len() int { return len(m.streamers) }

// mix streams up to len(out) stereo frames into out, summing the output of
// every queued streamer, scaling by volume, and removing drained streamers.
// out is always fully overwritten: frames not produced by any streamer are
// left as zero (silence). When the mixer is paused, mix leaves out untouched
// (the caller is responsible for ensuring out is already zeroed in that
// case).
//
// The tmp buffer is reused across calls to avoid allocations in the audio
// callback path.
func (m *mixer) mix(out [][2]float64, tmp *[512][2]float64) {
	if m.paused || len(m.streamers) == 0 || len(out) == 0 {
		return
	}
	vol := m.volume

	for len(out) > 0 {
		n := min(len(tmp), len(out))
		chunk := out[:n]

		// Zero the destination chunk so we can sum into it.
		for i := range chunk {
			chunk[i] = [2]float64{}
		}

		// Iterate by index, skipping forward only when the streamer survives.
		for si := 0; si < len(m.streamers); {
			// Zero the scratch buffer for this streamer's output.
			for i := range n {
				tmp[i] = [2]float64{}
			}

			sn, sok := m.streamers[si].Stream(tmp[:n])
			for i := range sn {
				chunk[i][0] += tmp[i][0] * vol
				chunk[i][1] += tmp[i][1] * vol
			}

			// A streamer that returned fewer than n frames or reported
			// !ok is drained and must be removed. Replace it with the
			// last streamer and re-examine this slot without advancing.
			if sn < n || !sok {
				last := len(m.streamers) - 1
				m.streamers[si] = m.streamers[last]
				m.streamers[last] = nil
				m.streamers = m.streamers[:last]
				if len(m.streamers) == 0 {
					break
				}
				continue
			}
			si++
		}

		out = out[n:]
	}
}
