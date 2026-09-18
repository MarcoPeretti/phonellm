// Package realtime is a minimal client for the OpenAI Realtime API over WebSocket,
// specialised for telephony: it speaks G.711 end to end so no transcoding is needed.
package realtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	endpoint = "wss://api.openai.com/v1/realtime"
	// A single G.711 frame is small; deltas are larger but still modest. 1 MiB is
	// generous headroom against the largest event we expect.
	readLimit = 1 << 20
)

type Options struct {
	APIKey       string
	Model        string
	Voice        string
	Instructions string
	// Format is derived from the codec the SIP leg actually negotiated, so the audio
	// path stays a byte passthrough whichever of PCMA/PCMU the Fritz!Box picked.
	Format AudioFormat
	Logger *slog.Logger
}

type Client struct {
	conn   *websocket.Conn
	opts   Options
	log    *slog.Logger
	events chan ServerEvent

	writeMu   sync.Mutex
	closeOnce sync.Once
}

// Dial connects, configures the session and blocks until the server has echoed a
// session.updated carrying the audio format we asked for.
func Dial(ctx context.Context, opts Options) (*Client, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	url := fmt.Sprintf("%s?model=%s", endpoint, opts.Model)
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + opts.APIKey}},
	})
	if err != nil {
		return nil, fmt.Errorf("realtime dial: %w", err)
	}
	conn.SetReadLimit(readLimit)

	c := &Client{
		conn:   conn,
		opts:   opts,
		log:    opts.Logger,
		events: make(chan ServerEvent, 64),
	}

	if err := c.configure(ctx); err != nil {
		conn.Close(websocket.StatusInternalError, "configure failed")
		return nil, err
	}
	go c.readLoop(ctx)
	return c, nil
}

// configure sends session.update and verifies the echo. Streaming A-law into a session
// that has quietly reverted to pcm16 produces loud static rather than an error, so this
// assertion is the difference between a working call and an unexplainable one.
func (c *Client) configure(ctx context.Context) error {
	cfg := &sessionConfig{
		Type:         "realtime",
		Instructions: c.opts.Instructions,
		Audio: sessionAudio{
			Input: inputAudio{
				Format: c.opts.Format,
				TurnDetection: &turnDetection{
					Type:              "server_vad",
					Threshold:         0.5,
					PrefixPaddingMS:   300,
					SilenceDurationMS: 500,
				},
				Transcription: &transcription{Model: "whisper-1"},
			},
			Output: outputAudio{Format: c.opts.Format, Voice: c.opts.Voice},
		},
	}
	if err := c.send(ctx, clientEvent{Type: evtSessionUpdate, Session: cfg}); err != nil {
		return err
	}

	deadline, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		ev, err := c.read(deadline)
		if err != nil {
			return fmt.Errorf("waiting for session.updated: %w", err)
		}
		switch ev.Type {
		case EvtError:
			return fmt.Errorf("realtime error during setup: %s", errText(ev))
		case EvtSessionUpdated:
			var echo sessionEcho
			if err := json.Unmarshal(ev.Session, &echo); err != nil {
				return fmt.Errorf("decoding session.updated: %w", err)
			}
			in, out := echo.Audio.Input.Format.Type, echo.Audio.Output.Format.Type
			if in != c.opts.Format.Type || out != c.opts.Format.Type {
				return fmt.Errorf(
					"session did not honour audio format: asked %q, got input=%q output=%q "+
						"(streaming G.711 into a PCM session would produce static)",
					c.opts.Format.Type, in, out)
			}
			c.log.Info("realtime session configured", "format", c.opts.Format.Type, "model", c.opts.Model)
			return nil
		}
	}
}

// Events yields decoded server events until the connection closes.
func (c *Client) Events() <-chan ServerEvent { return c.events }

func (c *Client) readLoop(ctx context.Context) {
	defer close(c.events)
	for {
		ev, err := c.read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				c.log.Debug("realtime read loop ended", "error", err)
			}
			return
		}
		select {
		case c.events <- ev:
		case <-ctx.Done():
			return
		}
	}
}

func (c *Client) read(ctx context.Context) (ServerEvent, error) {
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		return ServerEvent{}, err
	}
	var ev ServerEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return ServerEvent{}, fmt.Errorf("decoding server event: %w", err)
	}
	ev.Raw = data
	return ev, nil
}

func (c *Client) send(ctx context.Context, ev clientEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	// The audio pump and control paths both write; the connection is not write-safe.
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.Write(ctx, websocket.MessageText, b)
}

// AppendAudio streams one chunk of encoded caller audio (G.711 bytes, as taken
// straight off the wire) to the model.
func (c *Client) AppendAudio(ctx context.Context, frame []byte) error {
	return c.send(ctx, clientEvent{
		Type:  evtInputAudioAppend,
		Audio: base64.StdEncoding.EncodeToString(frame),
	})
}

// CreateResponse asks the model to speak unprompted. Used once on answer so the
// assistant greets the caller rather than waiting in silence.
func (c *Client) CreateResponse(ctx context.Context) error {
	return c.send(ctx, clientEvent{Type: evtResponseCreate})
}

// CancelResponse stops in-flight generation. Sent on barge-in.
func (c *Client) CancelResponse(ctx context.Context) error {
	return c.send(ctx, clientEvent{Type: evtResponseCancel})
}

func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.conn.Close(websocket.StatusNormalClosure, "call ended")
	})
	return err
}

// DecodeAudioDelta decodes the base64 payload of a response.output_audio.delta.
func DecodeAudioDelta(ev ServerEvent) ([]byte, error) {
	return base64.StdEncoding.DecodeString(ev.Delta)
}

func errText(ev ServerEvent) string {
	if ev.Error == nil {
		return "unknown error"
	}
	return fmt.Sprintf("%s (%s)", ev.Error.Message, ev.Error.Code)
}
