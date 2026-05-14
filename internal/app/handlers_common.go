package app

import (
	"encoding/json"
	"io"
	"net/http"
)

// readJSON decodes the request body into target. An empty body decodes
// as the zero value (for handlers that accept "no body" as "use
// defaults") rather than EOF.
//
// Deprecated: prefer decodeJSON (handlers_stream.go) for new code — it
// reads the body once via json.NewDecoder and rejects trailing data.
// readJSON remains for handlers that still need the legacy
// "read-then-unmarshal-twice" pattern (e.g. /api/events dispatch).
func readJSON(r *http.Request, target interface{}) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return json.Unmarshal([]byte("{}"), target)
	}
	return json.Unmarshal(data, target)
}

// readBody returns the raw request body bytes. Used by the rare handler
// that needs to peek at a discriminator before deciding which typed DTO
// to decode into (see /api/events POST). Empty bodies return "{}" so
// downstream json.Unmarshal calls succeed with zero values.
func readBody(r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return []byte("{}"), nil
	}
	return data, nil
}
