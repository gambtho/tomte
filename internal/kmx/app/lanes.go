package app

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// Lanes are how `kmx up` overlaps the parts of a bring-up that do not
// depend on each other.
//
// The journey is a chain only where the cluster forces it to be one: Ollama
// pulling a 1.9GB model and kagent's five pods pulling their images are
// independent from the moment the API server answers, and so are the two
// agents once the controller is up. Running them one after another cost CI a
// measured minute per run (W25) for no added proof — the same commands run,
// the same waits are satisfied, in the same cluster.
//
// What a lane must NOT do is make the output unreadable. Each lane writes
// through a prefixWriter, so a developer watching `kmx up` sees live progress
// attributed to the lane it came from ("[kagent] pod/... condition met")
// rather than two commands' output shuffled together.

// lane is one branch of an overlapped step: a label for its output, and the
// work to run against a lane-local App.
type lane struct {
	name string
	fn   func(*App) error
}

// prefixWriter tags every line it forwards with a lane's name, under a lock
// shared by all lanes writing to the same stream, so a line from one lane
// never lands inside a line from another.
//
// Both '\n' and '\r' end a line: `ollama pull` redraws its progress with
// carriage returns, and buffering all of that into one enormous line would
// hide the download while it is happening. A partial line left over when the
// lane ends is flushed with a newline of its own.
type prefixWriter struct {
	mu     *sync.Mutex
	out    io.Writer
	prefix string
	buf    []byte
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b == '\n' || b == '\r' {
			w.emit()
			continue
		}
		w.buf = append(w.buf, b)
	}
	return len(p), nil
}

// emit writes the buffered line, prefix and all. An empty line is still a
// line — dropping it would silently reflow output that was deliberately
// spaced — but a line that is only a line ending prints as the prefix alone,
// which is noise, so those are dropped.
func (w *prefixWriter) emit() {
	if len(w.buf) == 0 {
		return
	}
	fmt.Fprintf(w.out, "%s%s\n", w.prefix, w.buf)
	w.buf = w.buf[:0]
}

func (w *prefixWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.emit()
}

// runLanes runs every lane at once and returns after all of them have
// finished, joining whatever they failed with.
//
// Every lane is run, even after one fails: they are already in flight
// against the same cluster, and abandoning a half-finished helm install or
// model pull would leave the cluster in a state the next command cannot
// explain. The joined error names every lane that failed.
//
// A single lane runs in place, unprefixed — that is the ordinary path for
// `kmx up --step <one>` and keeps its output byte-identical to the serial
// version.
func (a *App) runLanes(lanes []lane) error {
	if len(lanes) == 0 {
		return nil
	}
	if len(lanes) == 1 {
		return lanes[0].fn(a)
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make([]error, len(lanes))
	writers := make([]*prefixWriter, len(lanes))

	for i, l := range lanes {
		w := &prefixWriter{mu: &mu, out: a.Err, prefix: "[" + l.name + "] "}
		writers[i] = w

		// A lane-local App: same configuration, same cluster, its own
		// streams. The guard has already run for the whole invocation
		// (Up guards before any lane starts), and a lane must never
		// prompt — nothing is reading its stdin.
		b := *a
		r := *a.Run
		r.Stdout, r.Stderr = w, w
		b.Run = &r
		b.Out, b.Err = w, w
		b.guarded = true

		wg.Add(1)
		go func(i int, l lane, b App) {
			defer wg.Done()
			if err := l.fn(&b); err != nil {
				errs[i] = fmt.Errorf("%s: %w", l.name, err)
			}
		}(i, l, b)
	}
	wg.Wait()
	for _, w := range writers {
		w.flush()
	}
	return errors.Join(errs...)
}
