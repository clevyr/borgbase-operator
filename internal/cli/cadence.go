package cli

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// cadence describes how often a cron expression fires, independent of which
// minute it happens to land on.
//
// Migration deliberately hands a hand-jittered schedule back to the operator as
// a cadence, which moves the time. Comparing expressions verbatim would then
// report a difference for every app and hide the ones that matter, so parity
// asks the question that actually matters: does it still fire just as often?
type cadence struct {
	fires int
	gaps  []int
}

// equal reports whether two schedules fire equally often, with the same spacing.
func (c cadence) equal(other cadence) bool {
	return c.fires == other.fires && slices.Equal(c.gaps, other.gaps)
}

func (c cadence) String() string {
	if c.fires == 0 {
		return "never"
	}
	if len(c.gaps) == 0 {
		return fmt.Sprintf("%d times per 28 days", c.fires)
	}
	if c.gaps[0] == c.gaps[len(c.gaps)-1] {
		return fmt.Sprintf("every %s", time.Duration(c.gaps[0])*time.Minute)
	}
	return fmt.Sprintf("%d times per 28 days, %s to %s apart", c.fires,
		time.Duration(c.gaps[0])*time.Minute, time.Duration(c.gaps[len(c.gaps)-1])*time.Minute)
}

// cadenceOf expands a five-field cron expression over 28 days and records how
// often it fires and how far apart.
//
// Note that when both day-of-month and day-of-week are restricted, real cron
// ORs them where this ANDs. Migration only ever converts expressions whose
// day, month and weekday fields are all "*", and a schedule it leaves pinned is
// compared against itself, so the difference cannot arise here.
func cadenceOf(expr string) (cadence, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return cadence{}, fmt.Errorf("cron expression %q has %d fields, want 5", expr, len(fields))
	}

	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	matchers := make([]func(int) bool, 5)
	for i, f := range fields {
		m, err := fieldMatcher(f, bounds[i][0], bounds[i][1])
		if err != nil {
			return cadence{}, fmt.Errorf("field %d of %q: %w", i+1, expr, err)
		}
		matchers[i] = m
	}

	// Starts on a Monday, and 28 days covers every weekday and a whole set of
	// month days.
	start := time.Date(2026, time.January, 5, 0, 0, 0, 0, time.UTC)
	const window = 28 * 24 * 60

	var fired []int
	for m := range window {
		t := start.Add(time.Duration(m) * time.Minute)
		if matchers[0](t.Minute()) && matchers[1](t.Hour()) && matchers[2](t.Day()) &&
			matchers[3](int(t.Month())) && matchers[4](int(t.Weekday())) {
			fired = append(fired, m)
		}
	}

	gaps := make([]int, 0, len(fired))
	for i := 1; i < len(fired); i++ {
		gaps = append(gaps, fired[i]-fired[i-1])
	}
	slices.Sort(gaps)
	return cadence{fires: len(fired), gaps: gaps}, nil
}

// fieldMatcher builds a predicate for one cron field.
func fieldMatcher(field string, minValue, maxValue int) (func(int) bool, error) {
	allowed := map[int]bool{}

	for part := range strings.SplitSeq(field, ",") {
		step := 1
		if base, stepStr, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("bad step %q", stepStr)
			}
			step = n
			part = base
		}

		lo, hi := minValue, maxValue
		switch {
		case part == "*":
		case strings.Contains(part, "-"):
			a, b, _ := strings.Cut(part, "-")
			var err error
			if lo, err = strconv.Atoi(a); err != nil {
				return nil, fmt.Errorf("bad range start %q", a)
			}
			if hi, err = strconv.Atoi(b); err != nil {
				return nil, fmt.Errorf("bad range end %q", b)
			}
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("bad value %q", part)
			}
			lo, hi = n, n
		}

		if lo < minValue || hi > maxValue || lo > hi {
			return nil, fmt.Errorf("%q is outside %d-%d", part, minValue, maxValue)
		}
		for v := lo; v <= hi; v += step {
			allowed[v] = true
		}
	}

	return func(v int) bool { return allowed[v] }, nil
}
