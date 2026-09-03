package backup

import (
	"regexp"
	"strings"
	"testing"
)

func TestResolveScheduleVerbatim(t *testing.T) {
	// An explicit cron expression must survive migration unchanged, so that
	// moving an app onto the operator does not move when it backs up.
	for _, expr := range []string{
		testSchedule, "36 * * * *", "58 * * * *",
		"27 */6 * * *", "29 */6 * * *",
		"0 1 * * *", "3 0 * * *", "20 0 * * *", "30 0 * * *",
	} {
		got, err := ResolveSchedule(expr, "any/key")
		if err != nil {
			t.Fatalf("ResolveSchedule(%q) error = %v", expr, err)
		}
		if got != expr {
			t.Errorf("ResolveSchedule(%q) = %q, want it unchanged", expr, got)
		}
	}
}

func TestResolveScheduleRejectsMalformedCron(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "* * * * * *", "  "} {
		if _, err := ResolveSchedule(expr, "k"); err == nil {
			t.Errorf("ResolveSchedule(%q) expected an error", expr)
		}
	}
}

func TestResolveScheduleShorthands(t *testing.T) {
	tests := []struct {
		schedule string
		pattern  string
	}{
		{"@hourly", `^\d+ \* \* \* \*$`},
		{"@daily", `^\d+ \d+ \* \* \*$`},
		{"@weekly", `^\d+ \d+ \* \* \d$`},
		{"@monthly", `^\d+ \d+ \d+ \* \*$`},
		{"@every 6h", `^\d+ \*/6 \* \* \*$`},
		{"@every 15m", `^\*/15 \* \* \* \*$`},
		{"@every 1h", `^\d+ \* \* \* \*$`},
	}
	for _, tt := range tests {
		got, err := ResolveSchedule(tt.schedule, "myapp-prod/restic")
		if err != nil {
			t.Fatalf("ResolveSchedule(%q) error = %v", tt.schedule, err)
		}
		if !regexp.MustCompile(tt.pattern).MatchString(got) {
			t.Errorf("ResolveSchedule(%q) = %q, want match %q", tt.schedule, got, tt.pattern)
		}
	}
}

func TestResolveScheduleRejectsUnevenSteps(t *testing.T) {
	// A step that does not divide its period evenly produces an irregular gap
	// when cron restarts the sequence, which would quietly break the retention
	// assumptions of an hourly or six-hourly backup.
	for _, expr := range []string{"@every 7h", "@every 25m", "@every 90s", "@every 0h", "@every 48h", "@nonsense"} {
		if _, err := ResolveSchedule(expr, "k"); err == nil {
			t.Errorf("ResolveSchedule(%q) expected an error", expr)
		}
	}
}

func TestJitterIsStableAndDistributed(t *testing.T) {
	const key = "myapp-prod/restic"
	first, err := ResolveSchedule("@hourly", key)
	if err != nil {
		t.Fatalf("ResolveSchedule error = %v", err)
	}
	for range 100 {
		again, err := ResolveSchedule("@hourly", key)
		if err != nil {
			t.Fatalf("ResolveSchedule error = %v", err)
		}
		if again != first {
			t.Fatalf("jitter is not stable: got %q then %q", first, again)
		}
	}

	// The whole point of jitter is that copy-pasted backups do not stampede, so
	// a fleet-sized set of namespace-shaped keys must land on a good spread of
	// minutes.
	names := []string{
		"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
		"india", "juliett", "kilo", "lima", "mike", "november", "oscar", "papa",
		"quebec", "romeo", "sierra", "tango", "uniform", "victor", "whiskey",
		"xray", "yankee", "zulu",
	}
	namespaces := make([]string, 0, len(names)+5)
	for _, n := range names {
		namespaces = append(namespaces, n+"-prod")
	}
	for _, n := range names[:5] {
		namespaces = append(namespaces, n+"-staging")
	}

	minutes := map[string]int{}
	for _, ns := range namespaces {
		got, err := ResolveSchedule("@hourly", ns+"/restic")
		if err != nil {
			t.Fatalf("ResolveSchedule error = %v", err)
		}
		minutes[strings.Fields(got)[0]]++
	}
	// With 31 backups over 60 minutes some collisions are expected, but a
	// heavy pile-up would mean the hash is not spreading them.
	for minute, n := range minutes {
		if n > 3 {
			t.Errorf("minute %s has %d backups, want at most 3", minute, n)
		}
	}
	if len(minutes) < 20 {
		t.Errorf("%d namespaces landed on only %d distinct minutes, want at least 20",
			len(namespaces), len(minutes))
	}
}
