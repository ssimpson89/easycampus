package ffmpeg

import (
	"regexp"
	"strings"
)

// -----------------------------------------------------------------------------
// Redaction
// -----------------------------------------------------------------------------

// RedactedCredentialSentinel replaces credentials in URLs that get logged.
const RedactedCredentialSentinel = "REDACTED"

// secretParamNames is the keyword alternation for query / fragment
// parameters whose values are always sensitive. A parameter is scrubbed
// when its NAME contains one of these (case-insensitive), so
// access_token, api_key, X-Amz-Signature, auth_token, etc. all match.
const secretParamNames = `passphrase|password|passwd|pwd|token|key|secret|signature|sig|streamid|auth|credential`

// secretKVRegex scrubs name=value pairs in the query / fragment of a
// URL. Group 1 is the separator (or start), group 2 the parameter name.
// '#' terminates a value (fragment start) for ordinary URLs.
var secretKVRegex = regexp.MustCompile(`(?i)(^|[?&#;])([^=&#;?\s]*(?:` + secretParamNames + `)[^=&#;\s]*)=[^&#\s]+`)

// secretKVRegexSRT is the SRT flavour: SRT stream IDs use the "#!::"
// prefix (srt://host?streamid=#!::r=live,m=publish&passphrase=...), so
// '#' is part of the query, not a fragment delimiter.
var secretKVRegexSRT = regexp.MustCompile(`(?i)(^|[?&;])([^=&;?\s]*(?:` + secretParamNames + `)[^=&;\s]*)=[^&\s]+`)

// RedactURLCredentials returns u with embedded credentials replaced by
// the redaction sentinel. It works on the raw string (no url.Parse
// round-trip), so malformed escapes and SRT '#' stream IDs can't make a
// secret slip through a parse failure:
//
//   - userinfo (rtsp://user:pass@host/p -> rtsp://REDACTED:REDACTED@host/p)
//   - secret query/fragment params (?passphrase=X -> ?passphrase=REDACTED,
//     also token/key/secret/signature/sig/streamid/auth variants)
//   - RTMP(S) stream keys in the path (rtmp://host/app/KEY ->
//     rtmp://host/app/REDACTED)
func RedactURLCredentials(u string) string {
	if u == "" {
		return u
	}
	schemeIdx := strings.Index(u, "://")
	if schemeIdx <= 0 {
		return u
	}
	scheme := strings.ToLower(u[:schemeIdx])
	out := redactUserinfo(u, schemeIdx)

	// Query / fragment scrub on the tail only, so host and path are
	// never touched by the keyword match.
	if tailStart := strings.IndexAny(out, "?#"); tailStart >= 0 {
		re := secretKVRegex
		if scheme == "srt" {
			re = secretKVRegexSRT
		}
		tail := re.ReplaceAllString(out[tailStart:], "${1}${2}="+RedactedCredentialSentinel)
		out = out[:tailStart] + tail
	}

	if scheme == "rtmp" || scheme == "rtmps" {
		out = redactRTMPKey(out)
	}
	return out
}

// redactUserinfo replaces any user:pass@ in the authority component.
// The authority ends at the first '/', '?' or '#'; the LAST '@' inside
// it separates userinfo from host (matching url.Parse semantics).
func redactUserinfo(u string, schemeIdx int) string {
	rest := u[schemeIdx+3:]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	at := strings.LastIndex(rest[:end], "@")
	if at < 0 {
		return u
	}
	return u[:schemeIdx+3] + RedactedCredentialSentinel + ":" + RedactedCredentialSentinel + rest[at:]
}

// redactRTMPKey replaces the last path segment of an RTMP URL when the
// path has at least two segments (rtmp://host/app/streamkey). A bare
// rtmp://host/app has no key and is left alone.
func redactRTMPKey(u string) string {
	schemeIdx := strings.Index(u, "://")
	rest := u[schemeIdx+3:]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return u
	}
	pathStart := schemeIdx + 3 + slash
	pathEnd := len(u)
	if q := strings.IndexAny(u[pathStart:], "?#"); q >= 0 {
		pathEnd = pathStart + q
	}
	path := u[pathStart:pathEnd]
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) < 2 || segs[len(segs)-1] == "" {
		return u
	}
	last := strings.LastIndex(path, "/")
	if last == len(path)-1 { // trailing slash: key is the segment before it
		trimmed := strings.TrimRight(path, "/")
		last = strings.LastIndex(trimmed, "/")
		return u[:pathStart] + trimmed[:last+1] + RedactedCredentialSentinel + path[len(trimmed):] + u[pathEnd:]
	}
	return u[:pathStart] + path[:last+1] + RedactedCredentialSentinel + u[pathEnd:]
}

// urlInLogRegex matches URL-shaped substrings inside a log line.
var urlInLogRegex = regexp.MustCompile(`(?i)(rtmp|rtmps|rtsp|rtsps|srt|http|https|udp|rtp)://[^\s'"]+`)

// RedactURLsInLog scans a free-form log line for URLs and runs each one
// through RedactURLCredentials, preserving trailing punctuation.
func RedactURLsInLog(line string) string {
	return urlInLogRegex.ReplaceAllStringFunc(line, func(match string) string {
		trailing := ""
		for len(match) > 0 {
			last := match[len(match)-1]
			if last == ',' || last == '.' || last == ')' || last == ']' || last == ';' || last == ':' {
				trailing = string(last) + trailing
				match = match[:len(match)-1]
				continue
			}
			break
		}
		return RedactURLCredentials(match) + trailing
	})
}

// redactStreamKey replaces a stream key in a log line with <redacted>.
// Skipped when the key is empty or shorter than 8 characters (those are
// almost certainly not a real key — refusing to redact "abc" prevents
// matching common words and leaking surrounding context).
func redactStreamKey(line, key string) string {
	if key == "" || len(key) < 8 {
		return line
	}
	if !strings.Contains(line, key) {
		return line
	}
	return strings.ReplaceAll(line, key, "<redacted>")
}

// RedactArgsForLog returns a copy of argv with secrets scrubbed. Used to
// emit FFmpeg invocations into logs without leaking RTSP/SRT credentials
// or the stream key. The streamKey arg redacts a fixed sentinel value
// (live encode key); URL-shaped args go through RedactURLCredentials.
//
// `-y` is preserved verbatim — it's not a secret.
func RedactArgsForLog(argv []string, streamKey string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		// Same order as the stderr path: URLs first, then the key.
		redacted := RedactURLsInLog(a)
		redacted = redactStreamKey(redacted, streamKey)
		out[i] = redacted
	}
	return out
}
