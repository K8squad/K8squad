package main

import (
	"testing"
	"time"
)

// ISI-7484 WS-3: KSQUAD_RUN_EVENT_RETENTION parsing — default 7d, "0" disables
// (pre-ISI-7484 unbounded journal), explicit durations honored, garbage falls
// back to the default rather than silently disabling retention.
func TestRunEventRetentionFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want time.Duration
	}{
		{"unset defaults to 7d", nil, 7 * 24 * time.Hour},
		{"zero disables", map[string]string{"KSQUAD_RUN_EVENT_RETENTION": "0"}, 0},
		{"explicit duration", map[string]string{"KSQUAD_RUN_EVENT_RETENTION": "72h"}, 72 * time.Hour},
		{"garbage falls back", map[string]string{"KSQUAD_RUN_EVENT_RETENTION": "soon"}, 7 * 24 * time.Hour},
		{"negative falls back", map[string]string{"KSQUAD_RUN_EVENT_RETENTION": "-5h"}, 7 * 24 * time.Hour},
	}
	get := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runEventRetentionFromEnv(get(tc.env)); got != tc.want {
				t.Fatalf("retention = %s, want %s", got, tc.want)
			}
		})
	}
}
