//go:build !cgo

package main

import (
	"encoding/json"
	"errors"
)

// callHost is unavailable without cgo; the package still builds so the
// pure-Go logic can be tested with CGO_ENABLED=0.
func callHost(string, any) (json.RawMessage, error) {
	return nil, errors.New("host callbacks require cgo")
}
