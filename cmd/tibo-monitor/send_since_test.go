package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestParseSendSince(t *testing.T) {
	startedAt := time.Date(2026, time.July, 1, 12, 0, 0, 123456789, time.UTC)
	for _, tt := range []struct {
		name  string
		value string
		want  time.Time
	}{
		{"UTC", "2026-07-01T11:00:00Z", time.Date(2026, time.July, 1, 11, 0, 0, 0, time.UTC)},
		{"positive offset", "2026-07-01T19:00:00+08:00", time.Date(2026, time.July, 1, 11, 0, 0, 0, time.UTC)},
		{"negative offset", "2026-07-01T07:00:00-04:00", time.Date(2026, time.July, 1, 11, 0, 0, 0, time.UTC)},
		{"nanoseconds", "2026-07-01T11:00:00.987654321Z", time.Date(2026, time.July, 1, 11, 0, 0, 987654321, time.UTC)},
		{"offset nanoseconds at startup", "2026-07-01T20:00:00.123456789+08:00", startedAt},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSendSince(tt.value, startedAt)
			if err != nil || !got.Equal(tt.want) || got.Location() != time.UTC {
				t.Fatalf("got %v, %v; want %v in UTC", got, err, tt.want)
			}
		})
	}
}

func TestParseSendSinceRejectsInvalidValues(t *testing.T) {
	startedAt := time.Date(2026, time.July, 1, 12, 0, 0, 123456789, time.UTC)
	const invalid = "send-since must be a non-empty, non-zero RFC3339 timestamp with a timezone"
	const future = "send-since must not be later than startup time"
	for _, tt := range []struct {
		name  string
		value string
		want  string
	}{
		{"empty", "", invalid},
		{"zero", "0001-01-01T00:00:00Z", invalid},
		{"no timezone", "2026-07-01T11:00:00", invalid},
		{"invalid", "DUMMY_PRIVATE_INPUT", invalid},
		{"invalid date", "2026-02-30T11:00:00Z", invalid},
		{"future", "2026-07-01T12:00:01Z", future},
		{"one nanosecond after startup", "2026-07-01T12:00:00.123456790Z", future},
		{"offset future", "2026-07-01T08:00:00.123456790-04:00", future},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSendSince(tt.value, startedAt)
			if err == nil || err.Error() != tt.want || !got.IsZero() {
				t.Fatalf("got %v, %v; want zero time and %q", got, err, tt.want)
			}
		})
	}
}

func TestSendSinceValidationBeforeConfig(t *testing.T) {
	const invalid = "send-since must be a non-empty, non-zero RFC3339 timestamp with a timezone"
	const unsupported = "flag is not supported for this command"
	for _, tt := range []struct {
		name    string
		command string
		value   string
		want    string
	}{
		{"no timezone", "run", "2000-01-01T00:00:00", invalid},
		{"invalid", "run", "DUMMY_PRIVATE_INPUT", invalid},
		{"future", "run", "9999-01-01T00:00:00Z", "send-since must not be later than startup time"},
		{"explicit empty", "run", "", invalid},
		{"probe valid", "probe", "2000-01-01T00:00:00Z", unsupported},
		{"probe empty", "probe", "", unsupported},
		{"status valid", "status", "2000-01-01T00:00:00Z", unsupported},
		{"status empty", "status", "", unsupported},
		{"test-send valid", "test-send", "2000-01-01T00:00:00Z", unsupported},
		{"test-send empty", "test-send", "", unsupported},
		{"UTC accepted", "run", "2000-01-01T00:00:00Z", "cannot open configuration file"},
		{"offset accepted", "run", "2000-01-01T08:00:00+08:00", "cannot open configuration file"},
		{"nanoseconds accepted", "run", "2000-01-01T00:00:00.123456789Z", "cannot open configuration file"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{tt.command, "--send-since=" + tt.value, "--config", filepath.Join(t.TempDir(), "missing.yaml")}
			err := execute(context.Background(), args, logger())
			if err == nil || err.Error() != tt.want {
				t.Fatalf("got %v; want %q", err, tt.want)
			}
		})
	}
}

func TestRunAcceptsSendSinceWithDebug(t *testing.T) {
	args := []string{"run", "--debug", "--send-since", "2000-01-01T00:00:00Z", "--config", filepath.Join(t.TempDir(), "missing.yaml")}
	if err := execute(context.Background(), args, logger()); err == nil || err.Error() != "cannot open configuration file" {
		t.Fatalf("send-since and debug were not accepted together: %v", err)
	}
}
