package bridge

import (
	"bytes"
	"testing"
	"time"
)

func frameDur() time.Duration { return 20 * time.Millisecond }

// Padding between utterances is normal and must not be counted; padding *during* one is
// the thing that makes a reply sound choppy.
func TestStarvationOnlyCountsWhileSpeaking(t *testing.T) {
	b := New(Options{FrameSize: frame, FrameDur: frameDur(), Silence: silenceAlaw})
	dst := make([]byte, frame)

	// Idle: the model is not speaking, so silence here is expected.
	for range 10 {
		n := b.out.Take(dst, silenceAlaw)
		b.framesSent.Add(1)
		if n < len(dst) && b.speaking.Load() {
			b.starvedFrames.Add(1)
		}
	}
	if got := b.starvedFrames.Load(); got != 0 {
		t.Errorf("counted %d starved frames while idle, want 0", got)
	}

	// Mid-utterance with an empty buffer: the caller hears a gap.
	b.speaking.Store(true)
	for range 5 {
		n := b.out.Take(dst, silenceAlaw)
		b.framesSent.Add(1)
		if n < len(dst) && b.speaking.Load() {
			b.starvedFrames.Add(1)
		}
	}
	if got := b.starvedFrames.Load(); got != 5 {
		t.Errorf("counted %d starved frames mid-utterance, want 5", got)
	}
}

// A full frame of real audio is never starvation, however it arrived.
func TestFullFrameIsNotStarvation(t *testing.T) {
	b := New(Options{FrameSize: frame, FrameDur: frameDur(), Silence: silenceAlaw})
	b.speaking.Store(true)
	b.out.Write(bytes.Repeat([]byte{0x40}, frame*3))

	dst := make([]byte, frame)
	for range 3 {
		n := b.out.Take(dst, silenceAlaw)
		if n < len(dst) {
			b.starvedFrames.Add(1)
		}
	}
	if got := b.starvedFrames.Load(); got != 0 {
		t.Errorf("counted %d starved frames with a full buffer, want 0", got)
	}
}
