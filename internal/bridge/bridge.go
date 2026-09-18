// Package bridge joins the SIP audio path to the Realtime model session.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MarcoPeretti/phonellm/internal/realtime"
)

// G.711 silence: A-law and mu-law encode digital zero differently.
const (
	silenceAlaw byte = 0xD5
	silenceUlaw byte = 0xFF
)

// SilenceByte returns the encoded silence value for an RTP payload type.
func SilenceByte(payloadType uint8) byte {
	if payloadType == 8 { // PCMA
		return silenceAlaw
	}
	return silenceUlaw
}

type Options struct {
	Reader    io.Reader
	Writer    io.Writer
	Client    *realtime.Client
	FrameSize int           // bytes per packet (160 for G.711 at 20ms)
	FrameDur  time.Duration // 20ms
	Silence   byte
	// BufferDur bounds the output pacing buffer. It must hold a full model reply: the
	// Realtime API bursts a reply's audio faster than realtime, so a buffer shorter than
	// the longest reply drops the oldest bytes mid-utterance and sounds choppy. Zero
	// falls back to a conservative default.
	BufferDur time.Duration
	MaxCall   time.Duration
	// SilenceTimeout ends the call when the caller has said nothing for this long.
	SilenceTimeout time.Duration
	Logger         *slog.Logger
}

// Turn is one utterance in the call, in order.
type Turn struct {
	Speaker string // "caller" or "assistant"
	Text    string
}

type Result struct {
	Turns    []Turn
	Duration time.Duration
	Reason   string // why the call ended
}

// Transcript renders the turns as a readable dialogue.
func (r Result) Transcript() string {
	var b strings.Builder
	for _, t := range r.Turns {
		fmt.Fprintf(&b, "%s: %s\n", t.Speaker, t.Text)
	}
	return b.String()
}

type Bridge struct {
	opts Options
	log  *slog.Logger
	out  *audioBuffer

	mu        sync.Mutex
	turns     []Turn
	lastHeard time.Time

	// Call-quality counters. The pacing goroutine and the event goroutine both touch
	// these, so they are atomic rather than guarded by mu.
	speaking      atomic.Bool // the model is mid-utterance
	framesSent    atomic.Int64
	starvedFrames atomic.Int64 // frames padded with silence *while speaking*
	bargeIns      atomic.Int64
	selfBargeIns  atomic.Int64 // barge-in fired while the model itself was talking
}

func New(opts Options) *Bridge {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	// The buffer must hold a whole reply, because the Realtime API delivers a reply's
	// audio in a burst far faster than the 20 ms RTP clock drains it. Too small and the
	// overflow policy drops the oldest bytes mid-utterance, which the caller hears as
	// choppy speech. Barge-in flushes the buffer regardless, so a larger buffer costs
	// nothing in interruptibility.
	bufferDur := opts.BufferDur
	if bufferDur <= 0 {
		bufferDur = 10 * time.Second
	}
	capacity := opts.FrameSize * int(bufferDur/opts.FrameDur)
	return &Bridge{
		opts:      opts,
		log:       opts.Logger,
		out:       newAudioBuffer(capacity),
		lastHeard: time.Now(),
	}
}

// Run pumps audio in both directions until the call ends, the model session drops, or
// a guard fires. It always returns a Result, even on error, so the caller can still
// record whatever transcript was captured.
func (b *Bridge) Run(ctx context.Context) (Result, error) {
	start := time.Now()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if b.opts.MaxCall > 0 {
		var timer *time.Timer
		timer = time.AfterFunc(b.opts.MaxCall, func() {
			b.log.Warn("max call duration reached, hanging up", "limit", b.opts.MaxCall)
			cancel()
		})
		defer timer.Stop()
	}

	var wg sync.WaitGroup
	reason := make(chan string, 3)

	// Greet first: without this the assistant waits in silence for the caller to speak,
	// which on a phone line reads as a dead connection.
	if err := b.opts.Client.CreateResponse(ctx); err != nil {
		return b.result(start, "greeting failed"), fmt.Errorf("requesting greeting: %w", err)
	}

	wg.Add(3)
	go func() { defer wg.Done(); b.pumpCallerToModel(ctx, cancel, reason) }()
	go func() { defer wg.Done(); b.pumpModelToCaller(ctx, cancel, reason) }()
	go func() { defer wg.Done(); b.handleEvents(ctx, cancel, reason) }()

	wg.Wait()

	why := "call ended"
	select {
	case why = <-reason:
	default:
	}
	res := b.result(start, why)
	b.reportQuality()
	return res, nil
}

// pumpCallerToModel forwards RTP payload straight to the model. Reads block until the
// next packet arrives, so this loop is naturally paced by the caller's own RTP clock.
func (b *Bridge) pumpCallerToModel(ctx context.Context, cancel context.CancelFunc, reason chan<- string) {
	defer cancel()
	buf := make([]byte, b.opts.FrameSize*4)
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := b.opts.Reader.Read(buf)
		if n > 0 {
			if err := b.opts.Client.AppendAudio(ctx, buf[:n]); err != nil {
				if ctx.Err() == nil {
					b.log.Error("forwarding caller audio", "error", err)
					send(reason, "model connection lost")
				}
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				b.log.Error("reading caller audio", "error", err)
			}
			send(reason, "caller hung up")
			return
		}
	}
}

// pumpModelToCaller is the RTP clock. It writes exactly one frame per tick regardless
// of how the model's bursts arrive, padding with silence on underrun. Deviating from
// this cadence is what turns natural speech into choppy garbage.
func (b *Bridge) pumpModelToCaller(ctx context.Context, cancel context.CancelFunc, reason chan<- string) {
	defer cancel()
	ticker := time.NewTicker(b.opts.FrameDur)
	defer ticker.Stop()

	frame := make([]byte, b.opts.FrameSize)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n := b.out.Take(frame, b.opts.Silence)
			b.framesSent.Add(1)
			// Padding between utterances is normal; padding *during* one means the
			// model could not keep up with the wire and the caller hears a gap.
			if n < len(frame) && b.speaking.Load() {
				b.starvedFrames.Add(1)
			}
			if _, err := b.opts.Writer.Write(frame); err != nil {
				if ctx.Err() == nil {
					b.log.Error("writing audio to caller", "error", err)
					send(reason, "media write failed")
				}
				return
			}
		}
	}
}

func (b *Bridge) handleEvents(ctx context.Context, cancel context.CancelFunc, reason chan<- string) {
	defer cancel()

	idle := time.NewTicker(time.Second)
	defer idle.Stop()

	events := b.opts.Client.Events()
	for {
		select {
		case <-ctx.Done():
			return

		case <-idle.C:
			b.mu.Lock()
			quiet := time.Since(b.lastHeard)
			b.mu.Unlock()
			if b.opts.SilenceTimeout > 0 && quiet > b.opts.SilenceTimeout {
				b.log.Info("caller silent, ending call", "for", quiet.Round(time.Second))
				send(reason, "caller silent")
				return
			}

		case ev, ok := <-events:
			if !ok {
				send(reason, "model session closed")
				return
			}
			b.onEvent(ctx, ev, reason)
		}
	}
}

func (b *Bridge) onEvent(ctx context.Context, ev realtime.ServerEvent, reason chan<- string) {
	switch ev.Type {
	case realtime.EvtAudioDelta:
		audio, err := realtime.DecodeAudioDelta(ev)
		if err != nil {
			b.log.Error("decoding audio delta", "error", err)
			return
		}
		b.speaking.Store(true)
		b.out.Write(audio)

	case realtime.EvtAudioDone, realtime.EvtResponseDone:
		b.speaking.Store(false)

	case realtime.EvtSpeechStarted:
		// Barge-in: drop everything queued and stop generation, so the assistant goes
		// quiet within one frame rather than talking over the caller.
		//
		// A barge-in raised while the model is mid-utterance is the signature of the
		// handset acoustically echoing the assistant back into the line: server VAD
		// hears the assistant's own voice as the caller speaking and cuts it off. That
		// presents as replies that break off mid-sentence, so it is counted separately.
		selfInterrupt := b.speaking.Load()
		b.bargeIns.Add(1)
		if selfInterrupt {
			b.selfBargeIns.Add(1)
		}
		b.log.Info("barge-in", "pending_audio_bytes", b.out.Len(),
			"while_model_speaking", selfInterrupt)

		b.speaking.Store(false)
		b.out.Reset()
		if err := b.opts.Client.CancelResponse(ctx); err != nil {
			b.log.Debug("cancelling response on barge-in", "error", err)
		}
		b.touch()

	case realtime.EvtSpeechStopped:
		b.touch()

	case realtime.EvtInTranscript:
		b.addTurn("caller", ev.Transcript)
		b.touch()

	case realtime.EvtOutTranscript:
		b.addTurn("assistant", ev.Transcript)

	case realtime.EvtError:
		b.log.Error("realtime error", "raw", string(ev.Raw))
	}
}

// reportQuality summarises how the audio path actually behaved. Choppy or truncated
// replies are the symptom; these counters say which of the two causes it was.
func (b *Bridge) reportQuality() {
	sent := b.framesSent.Load()
	starved := b.starvedFrames.Load()
	dropped := b.out.Dropped()
	barge := b.bargeIns.Load()
	self := b.selfBargeIns.Load()

	var starvedPct float64
	if sent > 0 {
		starvedPct = float64(starved) / float64(sent) * 100
	}

	b.log.Info("call audio quality",
		"frames_sent", sent,
		"starved_frames", starved,
		"starved_pct", fmt.Sprintf("%.1f%%", starvedPct),
		"dropped_bytes", dropped,
		"barge_ins", barge,
		"self_barge_ins", self,
	)

	// Roughly one frame in fifty is audible as a stutter.
	if starvedPct > 2 {
		b.log.Warn("model audio arrived too slowly to fill the RTP clock",
			"starved_pct", fmt.Sprintf("%.1f%%", starvedPct),
			"hint", "replies will have sounded choppy; usually network latency to the API")
	}
	if dropped > 0 {
		b.log.Warn("dropped buffered audio during call", "bytes", dropped,
			"hint", "model outpaced the RTP clock for over two seconds")
	}
	if self > 0 {
		b.log.Warn("the model interrupted itself",
			"count", self,
			"hint", "the handset is echoing the assistant back into the line; replies "+
				"will have broken off mid-sentence. Raise VADThreshold or "+
				"VADSilenceMS to make server VAD less trigger-happy")
	}
}

func (b *Bridge) touch() {
	b.mu.Lock()
	b.lastHeard = time.Now()
	b.mu.Unlock()
}

func (b *Bridge) addTurn(speaker, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	b.mu.Lock()
	b.turns = append(b.turns, Turn{Speaker: speaker, Text: text})
	b.mu.Unlock()
	b.log.Info("transcript", "speaker", speaker, "text", text)
}

func (b *Bridge) result(start time.Time, reason string) Result {
	b.mu.Lock()
	defer b.mu.Unlock()
	turns := make([]Turn, len(b.turns))
	copy(turns, b.turns)
	return Result{Turns: turns, Duration: time.Since(start), Reason: reason}
}

func send(ch chan<- string, s string) {
	select {
	case ch <- s:
	default:
	}
}
