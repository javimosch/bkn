package server

import (
	"encoding/json"
	"io"
	"time"
)

func decodeInto(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
