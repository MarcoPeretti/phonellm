package bridge

import (
	"bytes"
	"testing"
)

const frame = 160

func TestTakePadsWithSilenceOnUnderrun(t *testing.T) {
	b := newAudioBuffer(frame * 10)
	b.Write([]byte{1, 2, 3})

	dst := make([]byte, frame)
	n := b.Take(dst, silenceAlaw)

	if n != 3 {
		t.Fatalf("got %d real bytes, want 3", n)
	}
	if !bytes.Equal(dst[:3], []byte{1, 2, 3}) {
		t.Errorf("audio prefix corrupted: %v", dst[:3])
	}
	for i := 3; i < frame; i++ {
		if dst[i] != silenceAlaw {
			t.Fatalf("byte %d = %#x, want A-law silence %#x", i, dst[i], silenceAlaw)
		}
	}
}

func TestTakeOnEmptyBufferIsAllSilence(t *testing.T) {
	b := newAudioBuffer(frame * 10)
	dst := make([]byte, frame)

	if n := b.Take(dst, silenceUlaw); n != 0 {
		t.Fatalf("got %d real bytes from an empty buffer, want 0", n)
	}
	if got := bytes.Count(dst, []byte{silenceUlaw}); got != frame {
		t.Fatalf("%d of %d bytes are mu-law silence, want all", got, frame)
	}
}

// A burst from the model must come back out in exact frame-sized pieces, in order:
// this is what keeps the RTP clock steady.
func TestBurstIsDrainedInOrderedFrames(t *testing.T) {
	b := newAudioBuffer(frame * 10)
	burst := make([]byte, frame*3)
	for i := range burst {
		burst[i] = byte(i % 251)
	}
	b.Write(burst)

	dst := make([]byte, frame)
	for f := range 3 {
		if n := b.Take(dst, silenceAlaw); n != frame {
			t.Fatalf("frame %d: got %d bytes, want a full frame", f, n)
		}
		want := burst[f*frame : (f+1)*frame]
		if !bytes.Equal(dst, want) {
			t.Fatalf("frame %d does not match the source burst", f)
		}
	}
	if b.Len() != 0 {
		t.Errorf("buffer should be drained, %d bytes left", b.Len())
	}
}

func TestOverflowDropsOldestAndCountsIt(t *testing.T) {
	b := newAudioBuffer(frame)
	b.Write(bytes.Repeat([]byte{0xAA}, frame))
	b.Write(bytes.Repeat([]byte{0xBB}, frame))

	if b.Len() != frame {
		t.Fatalf("buffer holds %d bytes, want it capped at %d", b.Len(), frame)
	}
	if b.Dropped() != frame {
		t.Errorf("dropped %d bytes, want %d", b.Dropped(), frame)
	}

	// The newest audio must survive: dropping the oldest is what bounds latency.
	dst := make([]byte, frame)
	b.Take(dst, silenceAlaw)
	if got := bytes.Count(dst, []byte{0xBB}); got != frame {
		t.Errorf("kept %d newest bytes, want %d (oldest should have been dropped)", got, frame)
	}
}

// Barge-in has to silence the assistant within a single frame.
func TestResetDiscardsEverythingPending(t *testing.T) {
	b := newAudioBuffer(frame * 10)
	b.Write(bytes.Repeat([]byte{0x7F}, frame*5))

	b.Reset()

	if b.Len() != 0 {
		t.Fatalf("%d bytes survived a reset, want 0", b.Len())
	}
	dst := make([]byte, frame)
	if n := b.Take(dst, silenceAlaw); n != 0 {
		t.Fatalf("got %d real bytes after reset, want silence only", n)
	}
}

func TestSilenceByteMatchesCodec(t *testing.T) {
	if got := SilenceByte(8); got != silenceAlaw {
		t.Errorf("PCMA silence = %#x, want %#x", got, silenceAlaw)
	}
	if got := SilenceByte(0); got != silenceUlaw {
		t.Errorf("PCMU silence = %#x, want %#x", got, silenceUlaw)
	}
}
