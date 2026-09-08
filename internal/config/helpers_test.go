package config

import (
	"bytes"
	"io"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func contains(data []byte, s string) bool { return bytes.Contains(data, []byte(s)) }
