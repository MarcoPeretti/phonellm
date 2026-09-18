package bridge

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/MarcoPeretti/phonellm/internal/realtime"
)

func newTestBridge() *Bridge {
	return New(Options{FrameSize: frame, FrameDur: frameDur(), Silence: silenceAlaw})
}

// A barge-in with no response in flight is the common case -- the caller simply takes
// their turn -- and cancelling then draws response_cancel_not_active back from the
// service. The nil Client makes an unwanted cancel a panic rather than a silent pass.
func TestNoCancelWhenNoResponseInFlight(t *testing.T) {
	b := newTestBridge()
	reason := make(chan string, 1)

	b.onEvent(context.Background(), realtime.ServerEvent{Type: realtime.EvtSpeechStarted}, reason)

	if b.bargeIns.Load() != 1 {
		t.Errorf("counted %d barge-ins, want 1", b.bargeIns.Load())
	}
}

// Generation finishes seconds before the handset has played the audio out, so a
// barge-in with audio still queued is the assistant being echoed back, not the caller
// taking a clean turn. Gating on b.speaking alone misses exactly that case.
func TestSelfBargeInCountsQueuedAudio(t *testing.T) {
	b := newTestBridge()
	reason := make(chan string, 1)
	ctx := context.Background()

	// The model is done generating, but the caller is still hearing the reply.
	b.speaking.Store(false)
	b.out.Write(bytes.Repeat([]byte{0x40}, frame*50))

	b.onEvent(ctx, realtime.ServerEvent{Type: realtime.EvtSpeechStarted}, reason)

	if got := b.selfBargeIns.Load(); got != 1 {
		t.Errorf("counted %d self barge-ins with audio still queued, want 1", got)
	}
	if got := b.bargeIns.Load(); got != 1 {
		t.Errorf("counted %d barge-ins, want 1", got)
	}

	// Genuinely idle: nothing queued, nothing generating.
	b.onEvent(ctx, realtime.ServerEvent{Type: realtime.EvtSpeechStarted}, reason)
	if got := b.selfBargeIns.Load(); got != 1 {
		t.Errorf("counted %d self barge-ins after a clean turn, want 1", got)
	}
	if got := b.bargeIns.Load(); got != 2 {
		t.Errorf("counted %d barge-ins, want 2", got)
	}
}

// Once a response ends, a later barge-in must not try to cancel it again.
func TestResponseLifecycleGatesCancel(t *testing.T) {
	b := newTestBridge()
	reason := make(chan string, 1)
	ctx := context.Background()

	b.onEvent(ctx, realtime.ServerEvent{Type: realtime.EvtResponseCreated}, reason)
	if !b.responseActive.Load() {
		t.Fatal("response.created did not mark a response in flight")
	}

	// audio.done ends the utterance but not the response.
	b.onEvent(ctx, realtime.ServerEvent{Type: realtime.EvtAudioDone}, reason)
	if b.speaking.Load() {
		t.Error("still speaking after audio.done")
	}
	if !b.responseActive.Load() {
		t.Error("audio.done cleared the in-flight response")
	}

	b.onEvent(ctx, realtime.ServerEvent{Type: realtime.EvtResponseDone}, reason)
	if b.responseActive.Load() {
		t.Fatal("response.done did not clear the in-flight response")
	}

	// A nil Client would panic if this tried to cancel.
	b.onEvent(ctx, realtime.ServerEvent{Type: realtime.EvtSpeechStarted}, reason)
}

// The late-cancel rejection is a benign race, not a call fault, so it must not reach
// the error log that real session faults are diagnosed from.
func TestLateCancelErrorIsNotFatal(t *testing.T) {
	b := newTestBridge()
	reason := make(chan string, 1)

	ev := realtime.ServerEvent{Type: realtime.EvtError}
	ev.Error = &struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Type: "invalid_request_error", Code: realtime.ErrCancelNotActive}

	b.onEvent(context.Background(), ev, reason)

	select {
	case why := <-reason:
		t.Fatalf("late cancel ended the call: %q", why)
	case <-time.After(10 * time.Millisecond):
	}
}
