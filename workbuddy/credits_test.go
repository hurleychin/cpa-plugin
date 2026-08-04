package main

import "testing"

func TestIsCreditsExhausted(t *testing.T) {
	cases := []struct {
		name string
		cr   *creditsSummary
		want bool
	}{
		{"nil", nil, false},
		{"remain>0", &creditsSummary{TotalRemain: 10}, false},
		{"remain0 used>0", &creditsSummary{TotalRemain: 0, TotalUsed: 5}, true},
		{"remain0 size>0", &creditsSummary{TotalRemain: 0, TotalSize: 100}, true},
		{"remain0 packages", &creditsSummary{TotalRemain: 0, Packages: []packageSummary{{Name: "x"}}}, true},
		{"remain0 no data", &creditsSummary{TotalRemain: 0, TotalUsed: 0}, false},
	}
	for _, tc := range cases {
		if got := isCreditsExhausted(tc.cr); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
