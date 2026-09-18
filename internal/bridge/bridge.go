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
}

func New(opts Options) *Bridge {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	// Two seconds of buffered speech is plenty: beyond that the assistant is talking
	// so far ahead of the wire that dropping is better than growing latency.
	capacity := opts.FrameSize * int(2*time.Second/opts.FrameDur)
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
	if d := b.out.Dropped(); d > 0 {
		b.log.Warn("dropped buffered audio during call", "bytes", d,
			"hint", "model outpaced the RTP clock; check network latency")
	}
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
			b.out.Take(frame, b.opts.Silence)
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
		b.out.Write(audio)

	case realtime.EvtSpeechStarted:
		// Barge-in: drop everything queued and stop generation, so the assistant goes
		// quiet within one frame rather than talking over the caller.
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
