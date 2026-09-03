package main

import "testing"

func TestBuildEnvelope(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "input only",
			env:  map[string]string{"KSQUAD_INPUT": "do the thing"},
			want: "do the thing",
		},
		{
			name: "system context is prepended with a blank-line separator",
			env: map[string]string{
				"KSQUAD_SYSTEM_CONTEXT": "you are an agent",
				"KSQUAD_INPUT":          "do the thing",
			},
			want: "you are an agent\n\ndo the thing",
		},
		{
			name: "empty system context is skipped (no leading separator)",
			env:  map[string]string{"KSQUAD_SYSTEM_CONTEXT": "", "KSQUAD_INPUT": "do the thing"},
			want: "do the thing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildEnvelope(func(k string) string { return tc.env[k] })
			if got != tc.want {
				t.Fatalf("buildEnvelope = %q, want %q", got, tc.want)
			}
		})
	}
}
