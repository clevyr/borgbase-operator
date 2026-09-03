package backup

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"
)

// ResolveSchedule turns a ScheduledBackup's schedule into a concrete cron
// expression for the generated CronJob.
//
// A five-field cron expression passes through untouched, so an existing
// hand-tuned schedule can be migrated verbatim. A shorthand such as "@hourly"
// is expanded with a jitter derived from key, spreading copy-pasted backups
// across the period instead of firing them all on the hour.
//
// The jitter is a pure function of key, so a given backup keeps its slot across
// restarts and re-reconciles; it only moves if the resource is renamed.
func ResolveSchedule(schedule, key string) (string, error) {
	schedule = strings.TrimSpace(schedule)
	if schedule == "" {
		return "", fmt.Errorf("schedule is empty")
	}

	if !strings.HasPrefix(schedule, "@") {
		if fields := strings.Fields(schedule); len(fields) != 5 {
			return "", fmt.Errorf("cron expression %q has %d fields, want 5", schedule, len(fields))
		}
		return schedule, nil
	}

	h := jitter(key)
	minute := h % 60
	hour := (h / 60) % 24

	switch {
	case schedule == "@hourly":
		return fmt.Sprintf("%d * * * *", minute), nil
	case schedule == "@daily" || schedule == "@midnight":
		return fmt.Sprintf("%d %d * * *", minute, hour), nil
	case schedule == "@weekly":
		return fmt.Sprintf("%d %d * * %d", minute, hour, (h/1440)%7), nil
	case schedule == "@monthly":
		// Days 1-28 only: a backup scheduled on the 29th through 31st would
		// silently skip most months.
		return fmt.Sprintf("%d %d %d * *", minute, hour, (h/1440)%28+1), nil
	case schedule == "@yearly" || schedule == "@annually":
		return fmt.Sprintf("%d %d %d %d *", minute, hour, (h/1440)%28+1, (h/40320)%12+1), nil
	case strings.HasPrefix(schedule, "@every "):
		return resolveEvery(strings.TrimSpace(strings.TrimPrefix(schedule, "@every ")), minute)
	default:
		return "", fmt.Errorf("unknown schedule shorthand %q", schedule)
	}
}

// resolveEvery expands "@every <duration>" into a stepped cron expression.
//
// Only durations that divide evenly into an hour or a day are accepted. Cron
// steps restart at the top of each period, so "@every 7h" would fire at 00:00,
// 07:00, 14:00, 21:00 and then again at 00:00 three hours later - an uneven gap
// that is almost never what someone means.
func resolveEvery(d string, minute int) (string, error) {
	dur, err := time.ParseDuration(d)
	if err != nil {
		return "", fmt.Errorf("parsing @every duration %q: %w", d, err)
	}
	switch {
	case dur <= 0:
		return "", fmt.Errorf("@every duration %q must be positive", d)

	case dur < time.Hour:
		mins := int(dur.Minutes())
		if mins == 0 || dur%time.Minute != 0 {
			return "", fmt.Errorf("@every duration %q must be a whole number of minutes", d)
		}
		if 60%mins != 0 {
			return "", fmt.Errorf("@every duration %q must divide evenly into an hour", d)
		}
		return fmt.Sprintf("*/%d * * * *", mins), nil

	case dur == time.Hour:
		return fmt.Sprintf("%d * * * *", minute), nil

	case dur < 24*time.Hour:
		hours := int(dur.Hours())
		if dur%time.Hour != 0 {
			return "", fmt.Errorf("@every duration %q must be a whole number of hours", d)
		}
		if 24%hours != 0 {
			return "", fmt.Errorf("@every duration %q must divide evenly into a day", d)
		}
		return fmt.Sprintf("%d */%d * * *", minute, hours), nil

	case dur == 24*time.Hour:
		return fmt.Sprintf("%d %d * * *", minute, (jitter(strconv.Itoa(minute))/60)%24), nil

	default:
		return "", fmt.Errorf("@every duration %q is longer than a day; use an explicit cron expression", d)
	}
}

// jitter returns a stable non-negative hash of key.
func jitter(key string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % (1 << 24))
}
