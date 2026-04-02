package main

import "testing"

func TestShouldRecoverJobs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		command string
		want    bool
	}{
		{command: "mcp", want: true},
		{command: "serve", want: true},
		{command: "run", want: false},
		{command: "status", want: false},
		{command: "events", want: false},
		{command: "resume", want: false},
		{command: " approve ", want: false},
	}

	for _, tc := range tests {
		if got := shouldRecoverJobs(tc.command); got != tc.want {
			t.Fatalf("shouldRecoverJobs(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}
