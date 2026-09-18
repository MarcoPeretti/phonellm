package realtime

import "encoding/json"

// Event names used on the Realtime WebSocket. Only the subset phonellm needs.
const (
	// Client -> server.
	evtSessionUpdate    = "session.update"
	evtInputAudioAppend = "input_audio_buffer.append"
	evtResponseCreate   = "response.create"
	evtResponseCancel   = "response.cancel"

	// Server -> client.
	EvtSessionCreated  = "session.created"
	EvtSessionUpdated  = "session.updated"
	EvtSpeechStarted   = "input_audio_buffer.speech_started"
	EvtSpeechStopped   = "input_audio_buffer.speech_stopped"
	EvtResponseCreated = "response.created"
	EvtAudioDelta      = "response.output_audio.delta"
	EvtAudioDone       = "response.output_audio.done"
	EvtOutTranscript   = "response.output_audio_transcript.done"
	EvtInTranscript    = "conversation.item.input_audio_transcription.completed"
	EvtResponseDone    = "response.done"
	EvtError           = "error"
)

// ErrCancelNotActive is returned when a response.cancel lands after generation has
// already finished. It is a benign race, not a call fault: the service emits
// speech_started, the response completes, and the cancel arrives too late.
const ErrCancelNotActive = "response_cancel_not_active"

// AudioFormat is the GA nested audio format descriptor. The GA API rejects the older
// flat "input_audio_format"/"output_audio_format" string fields.
type AudioFormat struct {
	Type string `json:"type"`           // audio/pcma, audio/pcmu or audio/pcm
	Rate int    `json:"rate,omitempty"` // only meaningful for audio/pcm
}

type turnDetection struct {
	Type              string  `json:"type"`
	Threshold         float64 `json:"threshold,omitempty"`
	PrefixPaddingMS   int     `json:"prefix_padding_ms,omitempty"`
	SilenceDurationMS int     `json:"silence_duration_ms,omitempty"`
}

type transcription struct {
	Model string `json:"model"`
}

type inputAudio struct {
	Format        AudioFormat    `json:"format"`
	TurnDetection *turnDetection `json:"turn_detection,omitempty"`
	Transcription *transcription `json:"transcription,omitempty"`
}

type outputAudio struct {
	Format AudioFormat `json:"format"`
	Voice  string      `json:"voice,omitempty"`
}

type sessionAudio struct {
	Input  inputAudio  `json:"input"`
	Output outputAudio `json:"output"`
}

type sessionConfig struct {
	Type         string       `json:"type"` // always "realtime"
	Model        string       `json:"model,omitempty"`
	Instructions string       `json:"instructions,omitempty"`
	Audio        sessionAudio `json:"audio"`
}

type clientEvent struct {
	Type    string         `json:"type"`
	Audio   string         `json:"audio,omitempty"`
	Session *sessionConfig `json:"session,omitempty"`
}

// ServerEvent is a decoded inbound event. Raw retains the full payload so callers can
// pull fields this struct does not model.
type ServerEvent struct {
	Type       string `json:"type"`
	Delta      string `json:"delta"`
	Transcript string `json:"transcript"`
	Error      *struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Session json.RawMessage `json:"session"`
	Raw     json.RawMessage `json:"-"`
}

// sessionEcho is the shape we assert against in session.updated, to catch the known
// failure mode where the service silently falls back to pcm16.
type sessionEcho struct {
	Audio struct {
		Input  struct{ Format AudioFormat } `json:"input"`
		Output struct{ Format AudioFormat } `json:"output"`
	} `json:"audio"`
}
