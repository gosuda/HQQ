// Package convert holds the value conversions that the hqq-sqlite drivers
// share: the mapping of database/sql arguments to the five SQLite storage
// classes, the TEXT form of times, and the metadata derived from declared
// column types. Both drivers (gosuda.org/hqq/sqlite and the generated-client
// driver under it) use it, so they cannot diverge.
package convert

import "time"

// TimeLayout is the TEXT form of a time.Time argument. Times keep their own
// offset, so the text sorts correctly within one offset and round-trips to the
// same instant.
const TimeLayout = "2006-01-02 15:04:05.999999999-07:00"

// timeLayouts are the TEXT forms read back as time.Time from DATE, DATETIME, and
// TIMESTAMP columns. Text without an offset is UTC. The first entry is
// TimeLayout.
var timeLayouts = []string{
	TimeLayout,
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04",
	"2006-01-02T15:04",
	"2006-01-02",
}

// ParseTime parses the TEXT of a DATE, DATETIME, or TIMESTAMP column. It reports
// false when no layout matches, and the caller keeps the text.
func ParseTime(s string) (time.Time, bool) {
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
