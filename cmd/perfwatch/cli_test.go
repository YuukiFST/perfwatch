package main_test

import (
	"regexp"
	"testing"
)

// versionLine masks what changes per build: the VCS-derived version, the Go release and the platform.
var versionLine = regexp.MustCompile(`perfwatch \S+ \(go\S+ \S+/\S+\)`)

func TestCLIRouting(t *testing.T) {
	cases := []struct {
		golden string
		args   []string
	}{
		{golden: "cli-help", args: []string{"help"}},
		{golden: "cli-unknown-command", args: []string{"bogus"}},
		{golden: "cli-version", args: []string{"version"}},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			got := runPerfwatch(t, nil, tc.args...).transcript()
			got = versionLine.ReplaceAllString(got, "perfwatch <version> (<go> <os>/<arch>)")
			assertGolden(t, tc.golden, got)
		})
	}
}
