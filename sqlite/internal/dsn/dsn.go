// Package dsn parses the DSN of the hqq-sqlite drivers:
//
//	hqq://<listener>?db=...&mode=...&timeout=...&fetch=...&busy=...&txlock=...&pragma.<name>=<value>
//
// Both drivers accept the same options. The package returns the raw values;
// each driver applies its own defaults and checks its own limits.
package dsn

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Mode is the open mode of the database. The zero value is read-write-create.
type Mode uint8

// Open modes.
const (
	ModeReadWriteCreate Mode = iota // mode=rwc
	ModeReadWrite                   // mode=rw
	ModeReadOnly                    // mode=ro
)

// TxLock is the BEGIN mode of transactions. The zero value is deferred.
type TxLock uint8

// Transaction modes.
const (
	TxDeferred TxLock = iota
	TxImmediate
	TxExclusive
)

// Pragma is a PRAGMA to apply when a session opens.
type Pragma struct {
	Name  string
	Value string
}

// Options are the values of a DSN. A field that the DSN does not set is zero.
type Options struct {
	Listener string
	Database string
	Mode     Mode
	Timeout  time.Duration
	Busy     time.Duration
	Fetch    int
	TxLock   TxLock
	Pragmas  []Pragma // sorted by name
}

// Parse parses a DSN. Unknown options are errors, so misspelled options are
// not ignored. Every option may appear once.
func Parse(dsn string) (Options, error) {
	var o Options
	u, err := url.Parse(dsn)
	if err != nil {
		return o, fmt.Errorf("sqlite: invalid DSN: %w", err)
	}
	if u.Scheme != "hqq" {
		return o, fmt.Errorf("sqlite: DSN scheme must be hqq, got %q", u.Scheme)
	}
	if u.Host == "" || u.Path != "" || u.User != nil {
		return o, errors.New("sqlite: DSN must be hqq://<listener>?<options>")
	}
	q := u.Query()
	var pragmas []Pragma
	for key, vals := range q {
		if !strings.HasPrefix(key, "pragma.") {
			continue
		}
		if len(vals) != 1 {
			return o, fmt.Errorf("sqlite: option %s must be given once", key)
		}
		pragmas = append(pragmas, Pragma{Name: strings.TrimPrefix(key, "pragma."), Value: vals[0]})
	}
	sort.Slice(pragmas, func(i, j int) bool { return pragmas[i].Name < pragmas[j].Name })
	o.Pragmas = pragmas
	for key, vals := range q {
		if strings.HasPrefix(key, "pragma.") {
			continue
		}
		if len(vals) != 1 {
			return o, fmt.Errorf("sqlite: option %s must be given once", key)
		}
		v := vals[0]
		switch key {
		case "db":
			o.Database = v
		case "mode":
			switch v {
			case "rwc", "":
				o.Mode = ModeReadWriteCreate
			case "rw":
				o.Mode = ModeReadWrite
			case "ro":
				o.Mode = ModeReadOnly
			default:
				return o, fmt.Errorf("sqlite: invalid mode %q", v)
			}
		case "timeout":
			o.Timeout, err = parseDuration(key, v)
		case "busy":
			o.Busy, err = parseDuration(key, v)
		case "fetch":
			o.Fetch, err = strconv.Atoi(v)
			if err != nil {
				err = fmt.Errorf("sqlite: invalid fetch %q", v)
			}
		case "txlock":
			switch strings.ToLower(v) {
			case "deferred", "":
				o.TxLock = TxDeferred
			case "immediate":
				o.TxLock = TxImmediate
			case "exclusive":
				o.TxLock = TxExclusive
			default:
				return o, fmt.Errorf("sqlite: invalid txlock %q", v)
			}
		default:
			return o, fmt.Errorf("sqlite: unknown DSN option %q", key)
		}
		if err != nil {
			return o, err
		}
	}
	if o.Database == "" {
		return o, errors.New("sqlite: DSN option db is required")
	}
	o.Listener = u.Host
	return o, nil
}

func parseDuration(key, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("sqlite: invalid %s %q: %w", key, v, err)
	}
	return d, nil
}
