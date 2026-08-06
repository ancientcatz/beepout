# beepout

`beepout` is a `beep/speaker`-style audio output for the
[`github.com/gopxl/beep`](https://github.com/gopxl/beep) ecosystem, using
[`github.com/gen2brain/malgo`](https://github.com/gen2brain/malgo) as the
backend.

It works with both `github.com/gopxl/beep` v1 and `github.com/gopxl/beep/v2`
from a single module. The library does not import either beep package, relying
instead on Go's structural typing.

## Features

- Compatible with beep v1 and v2
- Drop-in replacement for `beep/speaker`
- Powered by malgo (miniaudio) with configurable backend priority
- Supports Android (AAudio/OpenSL ES) and other malgo-supported platforms
- Thread-safe API

## Installation

```sh
go get github.com/ancientcatz/beepout
```

Requires Go 1.26+ with CGO enabled.

## Usage

```go
package main

import (
	"os"

	beep "github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/mp3"

	"github.com/ancientcatz/beepout"
	"github.com/gen2brain/malgo"
)

func main() {
	f, _ := os.Open("track.mp3")
	defer f.Close()

	streamer, format, _ := mp3.Decode(f)
	defer streamer.Close()

	s, _ := beepout.New(beepout.Config{
		SampleRate: int(format.SampleRate),
		BufferSize: 2048,
		Backends: []malgo.Backend{
			malgo.BackendAaudio,    // Android (preferred)
			malgo.BackendOpensl,    // Android (fallback)
			malgo.BackendPulseAudio,// Linux
			malgo.BackendCoreaudio, // macOS / iOS
			malgo.BackendWasapi,    // Windows
		},
	})
	defer s.Close()

	done := make(chan struct{})

	s.Play(beep.Seq(
		streamer,
		beep.Callback(func() {
			close(done)
		}),
	))

	<-done
}
```

To use beep v1, simply change the import paths from `github.com/gopxl/beep/v2`
to `github.com/gopxl/beep`. No other code changes are required.

## API

```go
type Streamer interface {
	Stream(samples [][2]float64) (n int, ok bool)
	Err() error
}

type Config struct {
	SampleRate int
	BufferSize int
	Backends   []malgo.Backend
	Volume     float64
	LogProc    malgo.LogProc
}

func New(cfg Config) (*Speaker, error)

func (s *Speaker) Close() error
func (s *Speaker) Play(streamers ...Streamer)
func (s *Speaker) Clear()
func (s *Speaker) Pause()
func (s *Speaker) Resume()
func (s *Speaker) Lock()
func (s *Speaker) Unlock()
func (s *Speaker) SampleRate() int
```

## License

MIT. See [LICENSE](LICENSE).
