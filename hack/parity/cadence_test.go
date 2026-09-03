package main

import "testing"

// The schedule shapes actually found in the fleet.
const (
	cronHourly     = "36 * * * *"
	cronDaily      = "20 0 * * *"
	cronSixHourly  = "27 */6 * * *"
	cronEvery15Min = "*/15 * * * *"
)

// Converting a hand-jittered schedule to a shorthand must preserve how often
// the backup runs. These are the exact conversions migrate.sh performs against
// the fleet, plus the jittered forms the operator renders back.
func TestCadenceSurvivesJittering(t *testing.T) {
	tests := []struct {
		name     string
		original string
		jittered string
	}{
		{"hourly", cronHourly, "7 * * * *"},
		{"hourly at the top of the hour", "0 * * * *", "59 * * * *"},
		{"daily", cronDaily, "33 20 * * *"},
		{"daily across midnight", "0 1 * * *", "58 23 * * *"},
		{"every six hours", cronSixHourly, "14 3-23/6 * * *"},
		{"every fifteen minutes", cronEvery15Min, "7-59/15 * * * *"},
		{"weekly", "0 3 * * 0", "41 17 * * 4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original, err := cadenceOf(tt.original)
			if err != nil {
				t.Fatalf("cadenceOf(%q) error = %v", tt.original, err)
			}
			jittered, err := cadenceOf(tt.jittered)
			if err != nil {
				t.Fatalf("cadenceOf(%q) error = %v", tt.jittered, err)
			}
			if !original.equal(jittered) {
				t.Errorf("%q runs %s but %q runs %s; jitter must not change how often a backup runs",
					tt.original, original, tt.jittered, jittered)
			}
		})
	}
}

// The whole point of the gate: a conversion that quietly backs up less often
// has to fail, not pass because the minute happens to match.
func TestCadenceCatchesRealChanges(t *testing.T) {
	tests := []struct {
		name string
		a, b string
	}{
		{"hourly became daily", cronHourly, "36 3 * * *"},
		{"daily became weekly", cronDaily, "20 0 * * 1"},
		{"six hourly became twelve hourly", cronSixHourly, "27 */12 * * *"},
		{"quarter hourly became half hourly", cronEvery15Min, "*/30 * * * *"},
		{"hourly became twice hourly", cronHourly, "6,36 * * * *"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := cadenceOf(tt.a)
			if err != nil {
				t.Fatal(err)
			}
			b, err := cadenceOf(tt.b)
			if err != nil {
				t.Fatal(err)
			}
			if a.equal(b) {
				t.Errorf("%q (%s) and %q (%s) were treated as the same cadence", tt.a, a, tt.b, b)
			}
		})
	}
}

// An uneven step leaves a gap the operator rejects outright, but parity has to
// be able to describe one if it meets it in a hand-written schedule.
func TestCadenceReportsUnevenSteps(t *testing.T) {
	c, err := cadenceOf("0 */7 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.gaps) == 0 || c.gaps[0] == c.gaps[len(c.gaps)-1] {
		t.Errorf("%s: expected an uneven spacing for a 7 hour step", c)
	}
}

func TestCadenceCounts(t *testing.T) {
	tests := []struct {
		expr  string
		fires int
	}{
		{cronHourly, 28 * 24},
		{cronDaily, 28},
		{cronSixHourly, 28 * 4},
		{cronEvery15Min, 28 * 24 * 4},
	}
	for _, tt := range tests {
		c, err := cadenceOf(tt.expr)
		if err != nil {
			t.Fatalf("cadenceOf(%q) error = %v", tt.expr, err)
		}
		if c.fires != tt.fires {
			t.Errorf("cadenceOf(%q).fires = %d, want %d", tt.expr, c.fires, tt.fires)
		}
	}
}

func TestCadenceRejectsMalformed(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "* * * * * *", "99 * * * *", "x * * * *", "0 */0 * * *"} {
		if _, err := cadenceOf(expr); err == nil {
			t.Errorf("cadenceOf(%q) expected an error", expr)
		}
	}
}
