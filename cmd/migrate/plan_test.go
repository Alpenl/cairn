package main

import "testing"

// The helper decides whether to spend a maintenance window from --plan-json, so
// the flag has to be recognised exactly and never inferred from the run flag.
func TestPlanOnlyIsItsOwnFlag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "flag", args: []string{planJSONFlag}, want: true},
		{name: "with other flags", args: []string{reportJSONFlag, planJSONFlag}, want: true},
		{name: "padded", args: []string{"  --plan-json  "}, want: true},
		{name: "absent", args: []string{reportJSONFlag}},
		{name: "near miss", args: []string{"--plan-json=1"}},
		{name: "none", args: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := wantsPlanOnly(tc.args); got != tc.want {
				t.Fatalf("wantsPlanOnly(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// A plan must be parsable even when the caller forgot --report-json: a plan
// nobody can read is not a plan.
func TestPlanOnlyAlwaysSpeaksJSON(t *testing.T) {
	t.Parallel()
	if !wantsJSONReport([]string{planJSONFlag}) && !wantsPlanOnly([]string{planJSONFlag}) {
		t.Fatal("--plan-json must select the JSON contract on its own")
	}
}
