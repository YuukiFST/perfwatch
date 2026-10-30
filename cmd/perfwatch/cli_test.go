package main_test

import (
	"os/exec"
	"regexp"
	"strings"
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
		{golden: "cli-no-args", args: nil},
		{golden: "cli-unknown-command", args: []string{"bogus"}},
		{golden: "cli-version", args: []string{"version"}},
		{golden: "cli-version-help", args: []string{"version", "-h"}},
		{golden: "cli-version-extra-arg", args: []string{"version", "extra"}},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			got := runPerfwatch(t, nil, tc.args...).transcript()
			got = versionLine.ReplaceAllString(got, "perfwatch <version> (<go> <os>/<arch>)")
			assertGolden(t, tc.golden, got)
		})
	}
}

// TestVersionMatchesBuildInfo checks the printed version against the toolchain's own
// reading of the binary's build info, which the golden file masks.
func TestVersionMatchesBuildInfo(t *testing.T) {
	out, err := exec.Command("go", "version", "-m", perfwatchBin).Output()
	if err != nil {
		t.Fatalf("go version -m: %v", err)
	}
	want := ""
	for _, line := range strings.Split(string(out), "\n") {
		if fields := strings.Fields(line); len(fields) >= 3 && fields[0] == "mod" {
			want = fields[2]
		}
	}
	if want == "" {
		t.Fatalf("no mod line in go version -m output:\n%s", out)
	}
	if want == "(devel)" {
		want = "dev"
	}
	got := runPerfwatch(t, nil, "version").stdout
	if !strings.HasPrefix(got, "perfwatch "+want+" (") {
		t.Errorf("perfwatch version printed %q, build info says %q", got, want)
	}
}
