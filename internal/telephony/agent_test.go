package telephony

import (
	"testing"

	"github.com/emiago/diago/media"
)

// The whole design rests on the negotiated codec being G.711, so the mapping onto the
// Realtime format must be exact and must refuse anything else loudly.
func TestRealtimeFormatMapsG711(t *testing.T) {
	tests := []struct {
		name  string
		codec media.Codec
		want  string
	}{
		{"A-law", media.CodecAudioAlaw, "audio/pcma"},
		{"mu-law", media.CodecAudioUlaw, "audio/pcmu"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := realtimeFormat(tt.codec)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Type != tt.want {
				t.Errorf("format = %q, want %q", got.Type, tt.want)
			}
			if got.Rate != 0 {
				t.Errorf("rate = %d, want 0 (rate is only meaningful for audio/pcm)", got.Rate)
			}
		})
	}
}

func TestRealtimeFormatRejectsNonG711(t *testing.T) {
	if _, err := realtimeFormat(media.CodecAudioOpus); err == nil {
		t.Fatal("expected Opus to be rejected: it breaks the passthrough design")
	}
}

// G.711 is one byte per sample, so a 20ms frame is 160 bytes. The pacing loop depends
// on this identity holding for whichever G.711 variant was negotiated.
func TestG711FrameSizeIs160Bytes(t *testing.T) {
	for _, c := range []media.Codec{media.CodecAudioAlaw, media.CodecAudioUlaw} {
		if got := int(c.SampleTimestamp()); got != 160 {
			t.Errorf("%s frame = %d bytes, want 160", c.Name, got)
		}
	}
}

func TestAnswerCodecsPinG711AndKeepDTMF(t *testing.T) {
	var hasAlaw, hasUlaw, hasDTMF bool
	for _, c := range answerCodecs {
		switch c.PayloadType {
		case media.CodecAudioAlaw.PayloadType:
			hasAlaw = true
		case media.CodecAudioUlaw.PayloadType:
			hasUlaw = true
		case media.CodecTelephoneEvent8000.PayloadType:
			hasDTMF = true
		default:
			t.Errorf("unexpected codec offered: %s", c.String())
		}
	}
	if !hasAlaw || !hasUlaw {
		t.Error("both G.711 variants should be offered")
	}
	if !hasDTMF {
		t.Error("telephone-event must stay in the offer or RFC 2833 DTMF is lost")
	}
}

func TestSanitizeMakesSafeFilenames(t *testing.T) {
	tests := map[string]string{
		"":              "unknown",
		"+4930123456":   "_4930123456",
		"../../etc/pwd": "______etc_pwd",
		"anonymous":     "anonymous",
	}
	for in, want := range tests {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
