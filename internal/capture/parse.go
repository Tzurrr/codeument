package capture

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// Record is what the shell hook sends on stdin, NUL separated:
//
//	cdm1 \0 command \0 exit \0 start \0 end \0 cwd \0 shell \0 session \0 pid \0
//
// start and end are unix seconds, optionally fractional.
type Record struct {
	Command   string
	ExitCode  int
	Start     time.Time
	End       time.Time
	CWD       string
	Shell     string
	SessionID string
	PID       int
}

// MaxRecordBytes caps how much of stdin is read.
const MaxRecordBytes = 64 * 1024

// ParseRecord reads one record from r.
func ParseRecord(r io.Reader) (Record, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxRecordBytes))
	if err != nil {
		return Record{}, err
	}
	return ParseRecordBytes(data)
}

// ParseRecordBytes parses a raw record.
func ParseRecordBytes(data []byte) (Record, error) {
	data = bytes.TrimRight(data, "\n")
	parts := bytes.Split(data, []byte{0})
	if len(parts) < 9 || string(parts[0]) != "cdm1" {
		return Record{}, errors.New("malformed record (expected cdm1 header and 8 fields)")
	}
	rec := Record{
		Command:   strings.TrimSpace(string(parts[1])),
		CWD:       string(parts[5]),
		Shell:     string(parts[6]),
		SessionID: string(parts[7]),
	}
	rec.ExitCode, _ = strconv.Atoi(strings.TrimSpace(string(parts[2])))
	rec.PID, _ = strconv.Atoi(strings.TrimSpace(string(parts[8])))
	var err error
	if rec.Start, err = parseEpoch(string(parts[3])); err != nil {
		return rec, fmt.Errorf("start: %w", err)
	}
	if rec.End, err = parseEpoch(string(parts[4])); err != nil {
		return rec, fmt.Errorf("end: %w", err)
	}
	if rec.End.Before(rec.Start) {
		rec.End = rec.Start
	}
	if rec.SessionID == "" {
		return rec, errors.New("empty session id")
	}
	return rec, nil
}

func parseEpoch(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	// Some locales print a comma as the decimal separator in $EPOCHREALTIME.
	s = strings.ReplaceAll(s, ",", ".")
	if s == "" {
		return time.Now(), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return time.Time{}, err
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9)), nil
}
