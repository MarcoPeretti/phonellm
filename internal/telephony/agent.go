// Package telephony owns the SIP leg: registering with the Fritz!Box as a LAN IP
// telephone, answering inbound calls, and handing the audio path to the bridge.
package telephony

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/MarcoPeretti/phonellm/internal/bridge"
	"github.com/MarcoPeretti/phonellm/internal/config"
	"github.com/MarcoPeretti/phonellm/internal/realtime"
)

// CallHandler is invoked after a call completes, with whatever transcript was captured.
type CallHandler func(ctx context.Context, caller string, res bridge.Result)

type Agent struct {
	cfg    *config.Config
	log    *slog.Logger
	onCall CallHandler
	ua     *sipgo.UserAgent
	dg     *diago.Diago
}

// answerCodecs pins the media to G.711 so the audio path stays a byte passthrough all
// the way to the model. telephone-event is kept so RFC 2833 DTMF still arrives; without
// it, "press 1" style input would be impossible to add later.
var answerCodecs = []media.Codec{
	media.CodecAudioAlaw,
	media.CodecAudioUlaw,
	media.CodecTelephoneEvent8000,
}

func NewAgent(cfg *config.Config, log *slog.Logger, onCall CallHandler) (*Agent, error) {
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(cfg.SIPUsername),
		sipgo.WithUserAgentHostname(cfg.BindHost),
	)
	if err != nil {
		return nil, fmt.Errorf("creating user agent: %w", err)
	}

	// The RTP port range is a package-level setting in diago's media layer, not a
	// per-instance option. Pinning it keeps the firewall rule on the host narrow.
	media.RTPPortStart = cfg.RTPPortMin
	media.RTPPortEnd = cfg.RTPPortMax

	dg := diago.NewDiago(ua,
		diago.WithLogger(log),
		diago.WithTransport(diago.Transport{
			Transport: "udp",
			BindHost:  cfg.BindHost,
			BindPort:  cfg.BindPort,
		}),
		diago.WithMediaConfig(diago.MediaConfig{Codecs: answerCodecs}),
	)

	return &Agent{cfg: cfg, log: log, onCall: onCall, ua: ua, dg: dg}, nil
}

// Run serves inbound calls and maintains the registration until ctx is cancelled.
// Register blocks and re-REGISTERs on the registrar's expiry; if it drops we retry with
// backoff rather than exiting, so a Fritz!Box reboot does not take the service down.
func (a *Agent) Run(ctx context.Context) error {
	defer a.ua.Close()

	registrar := sip.Uri{}
	uri := fmt.Sprintf("sip:%s@%s", a.cfg.SIPUsername, a.cfg.SIPRegistrar)
	if err := sip.ParseUri(uri, &registrar); err != nil {
		return fmt.Errorf("parsing registrar uri %q: %w", uri, err)
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- a.dg.Serve(ctx, a.handleCall)
	}()

	go a.registerLoop(ctx, registrar)

	select {
	case <-ctx.Done():
		return nil
	case err := <-serveErr:
		return err
	}
}

func (a *Agent) registerLoop(ctx context.Context, registrar sip.Uri) {
	backoff := time.Second
	const maxBackoff = time.Minute

	for ctx.Err() == nil {
		err := a.dg.Register(ctx, registrar, diago.RegisterOptions{
			Username: a.cfg.SIPUsername,
			Password: a.cfg.SIPPassword,
			Expiry:   5 * time.Minute,
			OnRegistered: func() {
				backoff = time.Second
				a.log.Info("registered with Fritz!Box",
					"registrar", a.cfg.SIPRegistrar, "user", a.cfg.SIPUsername)
			},
		})
		if ctx.Err() != nil {
			return
		}
		a.log.Error("registration lost, retrying", "error", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (a *Agent) handleCall(inDialog *diago.DialogServerSession) {
	caller := inDialog.FromUser()
	log := a.log.With("call_id", inDialog.ID, "caller", caller)
	log.Info("inbound call")

	ctx := inDialog.Context()
	start := time.Now()

	if err := a.serveCall(ctx, log, inDialog, caller); err != nil {
		log.Error("call failed", "error", err, "after", time.Since(start).Round(time.Millisecond))
	}
	log.Info("call finished", "duration", time.Since(start).Round(time.Second))
}

func (a *Agent) serveCall(ctx context.Context, log *slog.Logger, inDialog *diago.DialogServerSession, caller string) error {
	inDialog.Trying()
	inDialog.Ringing()

	// Ring for a moment before picking up: answering on the first millisecond sounds
	// robotic, and it leaves room for a human to grab the handset instead.
	if a.cfg.RingDelay > 0 {
		select {
		case <-time.After(a.cfg.RingDelay):
		case <-ctx.Done():
			return nil
		}
	}

	if err := inDialog.AnswerOptions(diago.AnswerOptions{Codecs: answerCodecs}); err != nil {
		return fmt.Errorf("answering: %w", err)
	}

	var rprops, wprops diago.MediaProps
	reader, err := inDialog.AudioReader(diago.WithAudioReaderMediaProps(&rprops))
	if err != nil {
		return fmt.Errorf("audio reader: %w", err)
	}
	writer, err := inDialog.AudioWriter(diago.WithAudioWriterMediaProps(&wprops))
	if err != nil {
		return fmt.Errorf("audio writer: %w", err)
	}

	codec := rprops.Codec
	log.Info("media negotiated", "codec", codec.String(), "remote", rprops.Raddr)

	format, err := realtimeFormat(codec)
	if err != nil {
		return err
	}

	// Recording wraps the audio path transparently: it tees a decoded copy into a
	// stereo WAV while passing the encoded bytes through untouched.
	if a.cfg.RecordDir != "" {
		recReader, recWriter, closeRec, path, err := a.startRecording(inDialog, caller)
		if err != nil {
			// A recording problem should not cost the caller their call.
			log.Error("recording disabled for this call", "error", err)
		} else {
			defer func() {
				if err := closeRec(); err != nil {
					log.Error("closing recording", "error", err)
				}
			}()
			reader, writer = recReader, recWriter
			log.Info("recording call", "file", path)
		}
	}

	if a.cfg.EchoTest {
		return a.echo(log, reader, writer)
	}

	client, err := realtime.Dial(ctx, realtime.Options{
		APIKey:       a.cfg.APIKey,
		Model:        a.cfg.Model,
		Voice:        a.cfg.Voice,
		Instructions: a.cfg.Instructions,
		Format:       format,
		Logger:       log,
	})
	if err != nil {
		return fmt.Errorf("connecting to realtime API: %w", err)
	}
	defer client.Close()

	b := bridge.New(bridge.Options{
		Reader:         reader,
		Writer:         writer,
		Client:         client,
		FrameSize:      int(codec.SampleTimestamp()),
		FrameDur:       codec.SampleDur,
		Silence:        bridge.SilenceByte(codec.PayloadType),
		MaxCall:        a.cfg.MaxCallTime,
		SilenceTimeout: a.cfg.SilenceTimeout,
		Logger:         log,
	})

	res, err := b.Run(ctx)
	log.Info("call summary", "reason", res.Reason, "turns", len(res.Turns))

	if a.onCall != nil && len(res.Turns) > 0 {
		// The call is over; do not let a slow notifier hold the SIP dialog open.
		notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		a.onCall(notifyCtx, caller, res)
	}
	return err
}

// echo streams the caller's own audio back to them. This is the milestone-0 gate: it
// proves registration, SDP, and RTP in both directions without involving an LLM.
func (a *Agent) echo(log *slog.Logger, r io.Reader, w io.Writer) error {
	log.Warn("ECHO TEST MODE: streaming caller audio back, no LLM involved")
	_, err := media.Copy(r, w)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// startRecording tees the call into a stereo WAV (caller left, assistant right) and
// returns the wrapped audio path. The recording struct holds a mutex and must never be
// copied, so it stays addressable here and only its reader/writer escape.
func (a *Agent) startRecording(inDialog *diago.DialogServerSession, caller string) (io.Reader, io.Writer, func() error, string, error) {
	if err := os.MkdirAll(a.cfg.RecordDir, 0o750); err != nil {
		return nil, nil, nil, "", err
	}
	name := fmt.Sprintf("%s_%s.wav", time.Now().Format("20060102-150405"), sanitize(caller))
	path := filepath.Join(a.cfg.RecordDir, name)

	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		return nil, nil, nil, "", err
	}
	rec, err := inDialog.AudioStereoRecordingCreate(file)
	if err != nil {
		file.Close()
		return nil, nil, nil, "", err
	}
	// Close order matters: the recorder must flush and interleave before the file goes.
	closeRec := func() error {
		return errors.Join(rec.Close(), file.Close())
	}
	return rec.AudioReader(), rec.AudioWriter(), closeRec, path, nil
}

// realtimeFormat maps the negotiated RTP payload type onto the Realtime API's audio
// format, so the model speaks exactly the codec already on the wire.
func realtimeFormat(c media.Codec) (realtime.AudioFormat, error) {
	switch c.PayloadType {
	case media.CodecAudioAlaw.PayloadType:
		return realtime.AudioFormat{Type: "audio/pcma"}, nil
	case media.CodecAudioUlaw.PayloadType:
		return realtime.AudioFormat{Type: "audio/pcmu"}, nil
	default:
		return realtime.AudioFormat{}, fmt.Errorf(
			"negotiated codec %s is not G.711; the passthrough design requires PCMA or PCMU", c.String())
	}
}

func sanitize(s string) string {
	if s == "" {
		return "unknown"
	}
	out := []rune(s)
	for i, r := range out {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			out[i] = '_'
		}
	}
	return string(out)
}
