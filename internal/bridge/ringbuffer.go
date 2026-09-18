package bridge

import "sync"

// audioBuffer is a bounded FIFO of encoded audio bytes sitting between the model and
// the RTP clock. The model emits audio in bursts; RTP demands one frame every 20ms.
// This buffer absorbs that mismatch, and can be flushed instantly on barge-in.
type audioBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
	// dropped counts bytes discarded on overflow, for diagnostics.
	dropped int
}

func newAudioBuffer(max int) *audioBuffer {
	return &audioBuffer{max: max}
}

// Write appends audio, discarding the oldest bytes if the cap is exceeded. Overflow
// means the model is generating faster than realtime for a sustained period; dropping
// the oldest keeps latency bounded rather than letting the caller fall behind forever.
func (b *audioBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = b.buf[over:]
		b.dropped += over
	}
	return len(p), nil
}

// Take fills dst with buffered audio, padding the remainder with silence. It reports
// how many real audio bytes were available, so the caller can tell a genuine underrun
// (0) from ordinary tail padding.
func (b *audioBuffer) Take(dst []byte, silence byte) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := copy(dst, b.buf)
	b.buf = b.buf[n:]
	for i := n; i < len(dst); i++ {
		dst[i] = silence
	}
	return n
}

// Reset discards everything pending. Called on barge-in so the assistant stops
// talking the instant the caller starts.
func (b *audioBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = b.buf[:0]
}

func (b *audioBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf)
}

func (b *audioBuffer) Dropped() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}
