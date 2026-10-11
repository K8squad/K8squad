package discussion

import "testing"

// ISI-5670: the per-discussion concurrent-run cap is env/Secret-configurable, defaulting to 4, and a bad
// override never silently disables the backstop.
func TestResolveMaxConcurrentRunsFromEnv(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"unset", "", DefaultMaxConcurrentRunsPerDiscussion},
		{"whitespace", "   ", DefaultMaxConcurrentRunsPerDiscussion},
		{"explicit", "6", 6},
		{"padded", "  8 ", 8},
		{"one", "1", 1},
		{"zero falls back", "0", DefaultMaxConcurrentRunsPerDiscussion},
		{"negative falls back", "-3", DefaultMaxConcurrentRunsPerDiscussion},
		{"non-numeric falls back", "lots", DefaultMaxConcurrentRunsPerDiscussion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveMaxConcurrentRunsFromEnv(tc.raw); got != tc.want {
				t.Fatalf("resolveMaxConcurrentRunsFromEnv(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

// The default is positive so the backstop is always armed, and the accessor reflects the resolved value.
func TestMaxConcurrentRunsPerDiscussionDefault(t *testing.T) {
	if DefaultMaxConcurrentRunsPerDiscussion <= 0 {
		t.Fatalf("default cap must be positive, got %d", DefaultMaxConcurrentRunsPerDiscussion)
	}
	if MaxConcurrentRunsPerDiscussion() <= 0 {
		t.Fatalf("live cap must be positive, got %d", MaxConcurrentRunsPerDiscussion())
	}
}
