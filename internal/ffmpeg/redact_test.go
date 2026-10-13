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
	// SRT stream IDs routinely carry credentials (#!::u=...,s=...).
	if !strings.Contains(got, "streamid=REDACTED") || strings.Contains(got, "publish:test") {
		t.Errorf("expected streamid to be redacted, got %q", got)
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
			want:   []string{"passphrase=REDACTED", "streamid=REDACTED"},
			banned: []string{"Hunter2", "streamid=foo"},
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
	// The RTMP key path segment is scrubbed by URL redaction even
	// before the explicit stream-key pass.
	if !strings.Contains(joined, "live2/REDACTED") {
		t.Errorf("expected RTMP key segment redacted: %s", joined)
	}
	if !strings.Contains(joined, "REDACTED:REDACTED") {
		t.Errorf("expected userinfo REDACTED: %s", joined)
	}
	// -y must still appear — we're not stripping operational args.
	if !strings.Contains(joined, "-y") {
		t.Errorf("expected -y to remain: %s", joined)
	}
}

func TestRedactURLCredentialsLeakCases(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   []string
		banned []string
	}{
		{
			name:   "srt streamid with #!:: prefix and passphrase after it",
			in:     "srt://relay:9000?streamid=#!::r=live/feed,m=publish,u=bob,s=S3cr3t&passphrase=Hunter22&latency=200",
			want:   []string{"srt://relay:9000?", "streamid=REDACTED", "passphrase=REDACTED", "latency=200"},
			banned: []string{"S3cr3t", "Hunter22", "u=bob"},
		},
		{
			name:   "srt passphrase only in fragment-looking tail",
			in:     "srt://relay:9000#passphrase=Hunter22",
			want:   []string{"passphrase=REDACTED"},
			banned: []string{"Hunter22"},
		},
		{
			name:   "bad escape in query does not bypass redaction",
			in:     "srt://relay:9000?mode=caller&x=%zz&passphrase=Hunter22",
			want:   []string{"passphrase=REDACTED", "x=%zz"},
			banned: []string{"Hunter22"},
		},
		{
			name:   "bad escape with userinfo",
			in:     "rtsp://admin:pa%zzss@cam.local/stream",
			want:   []string{"rtsp://REDACTED:REDACTED@cam.local/stream"},
			banned: []string{"pa%zzss"},
		},
		{
			name:   "presigned S3 / CDN tokens",
			in:     "https://bucket.s3.amazonaws.com/v.m3u8?X-Amz-Credential=AKIAX&X-Amz-Signature=abcdef123&access_token=tok123&sig=zz9&api_key=k1&expires=99",
			want:   []string{"X-Amz-Signature=REDACTED", "access_token=REDACTED", "sig=REDACTED", "api_key=REDACTED", "expires=99"},
			banned: []string{"abcdef123", "tok123", "zz9", "=k1", "AKIAX"},
		},
		{
			name:   "auth param",
			in:     "http://cdn/x.m3u8?auth=Zm9vOmJhcg",
			want:   []string{"auth=REDACTED"},
			banned: []string{"Zm9vOmJhcg"},
		},
		{
			name:   "rtmp key in path",
			in:     "rtmp://a.rtmp.youtube.com/live2/abcd-1234-efgh-5678",
			want:   []string{"rtmp://a.rtmp.youtube.com/live2/REDACTED"},
			banned: []string{"abcd-1234"},
		},
		{
			name:   "rtmps key in path with query",
			in:     "rtmps://live-api-s.facebook.com:443/rtmp/FB-123-0-Ab?s_bl=1",
			want:   []string{"rtmps://live-api-s.facebook.com:443/rtmp/REDACTED?s_bl=1"},
			banned: []string{"FB-123"},
		},
		{
			name: "rtmp app only, nothing to redact",
			in:   "rtmp://ingest.example.com/live",
			want: []string{"rtmp://ingest.example.com/live"},
		},
		{
			name: "host and path are not keyword-matched",
			in:   "https://keyserver.example.com/tokens/list.m3u8",
			want: []string{"https://keyserver.example.com/tokens/list.m3u8"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RedactURLCredentials(c.in)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("expected %q in %q", w, got)
				}
			}
			for _, b := range c.banned {
				if strings.Contains(got, b) {
					t.Errorf("leaked %q in %q", b, got)
				}
			}
		})
	}
}
