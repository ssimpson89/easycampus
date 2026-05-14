package ffmpeg

import (
	"strings"
	"testing"
)

func TestRedactURLCredentialsUserinfo(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{
			in:   "rtsp://admin:Sup3rSecret@cam.local:554/Streaming/Channels/101",
			want: "rtsp://REDACTED:REDACTED@cam.local:554/Streaming/Channels/101",
		},
		{
			in:   "rtsps://user:pass@host/path?other=keep",
			want: "rtsps://REDACTED:REDACTED@host/path?other=keep",
		},
		{
			in:   "http://anonymous-feed/stream.m3u8",
			want: "http://anonymous-feed/stream.m3u8",
		},
	}
	for _, c := range cases {
		got := RedactURLCredentials(c.in)
		if got != c.want {
			t.Errorf("RedactURLCredentials(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRedactURLCredentialsSecretQuery(t *testing.T) {
	in := "srt://relay.example.com:9999?streamid=publish:test&passphrase=Sup3rSecretPwd&latency=4000000"
	got := RedactURLCredentials(in)
	if !strings.Contains(got, "passphrase=REDACTED") {
		t.Errorf("expected passphrase to be redacted, got %q", got)
	}
	if !strings.Contains(got, "streamid=publish") {
		t.Errorf("expected streamid to be preserved, got %q", got)
	}
	if !strings.Contains(got, "latency=4000000") {
		t.Errorf("expected latency to be preserved, got %q", got)
	}
}

func TestRedactURLCredentialsEmpty(t *testing.T) {
	if RedactURLCredentials("") != "" {
		t.Error("expected empty input to round-trip empty")
	}
}

func TestRedactURLsInLog(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string
		banned   []string
	}{
		{
			name:   "rtsp connect-refused error with userinfo",
			in:     "[rtsp @ 0x123] method DESCRIBE failed: rtsp://admin:Sup3rSecret@cam.local:554/path",
			want:   []string{"rtsp://REDACTED:REDACTED@cam.local:554/path"},
			banned: []string{"Sup3rSecret", "admin:Sup"},
		},
		{
			name:   "srt url with passphrase in query",
			in:     "[srt] connection failed to srt://relay.example.com:9999?passphrase=Hunter2&streamid=foo",
			want:   []string{"passphrase=REDACTED", "streamid=foo"},
			banned: []string{"Hunter2"},
		},
		{
			name:   "trailing punctuation preserved",
			in:     "could not open 'rtsp://user:pass@host/path'.",
			want:   []string{"rtsp://REDACTED:REDACTED@host/path", "."},
			banned: []string{"user:pass"},
		},
		{
			name: "no URL -> unchanged",
			in:   "[h264 @ 0x123] mmco: unref short failure",
			want: []string{"mmco: unref short failure"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RedactURLsInLog(c.in)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("expected %q in result: %q", w, got)
				}
			}
			for _, b := range c.banned {
				if strings.Contains(got, b) {
					t.Errorf("did not expect %q in result: %q", b, got)
				}
			}
		})
	}
}

func TestRedactStreamKey(t *testing.T) {
	cases := []struct {
		name, line, key, want string
	}{
		{
			name: "rtmp URL containing key",
			line: "Failed to connect to rtmp://a.rtmp.youtube.com/live2/abcd-1234-efgh-5678",
			key:  "abcd-1234-efgh-5678",
			want: "Failed to connect to rtmp://a.rtmp.youtube.com/live2/<redacted>",
		},
		{
			name: "no key in line",
			line: "warning: dropped frame",
			key:  "abcd-1234-efgh-5678",
			want: "warning: dropped frame",
		},
		{
			name: "empty key skips redaction",
			line: "rtmp://example/live2/somekey",
			key:  "",
			want: "rtmp://example/live2/somekey",
		},
		{
			name: "short key skips redaction",
			line: "the abc rtmp://thing",
			key:  "abc",
			want: "the abc rtmp://thing",
		},
		{
			name: "multiple occurrences",
			line: "key=abcd-1234-efgh-5678 url=rtmp://x/abcd-1234-efgh-5678",
			key:  "abcd-1234-efgh-5678",
			want: "key=<redacted> url=rtmp://x/<redacted>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactStreamKey(tc.line, tc.key)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestRedactArgsForLogScrubsURLsAndKeys(t *testing.T) {
	argv := []string{
		"ffmpeg", "-y",
		"-i", "rtsp://admin:Sup3rSecret@cam.local:554/path",
		"-c:v", "libx264",
		"-f", "flv",
		"rtmp://a.rtmp.youtube.com/live2/MY-STREAM-KEY-123",
	}
	got := RedactArgsForLog(argv, "MY-STREAM-KEY-123")
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "Sup3rSecret") {
		t.Errorf("RTSP password leaked: %s", joined)
	}
	if strings.Contains(joined, "MY-STREAM-KEY-123") {
		t.Errorf("stream key leaked: %s", joined)
	}
	if !strings.Contains(joined, "<redacted>") {
		t.Errorf("expected <redacted> sentinel: %s", joined)
	}
	if !strings.Contains(joined, "REDACTED:REDACTED") {
		t.Errorf("expected userinfo REDACTED: %s", joined)
	}
	// -y must still appear — we're not stripping operational args.
	if !strings.Contains(joined, "-y") {
		t.Errorf("expected -y to remain: %s", joined)
	}
}
