package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/sqlite/internal/wire"
)

// DriverName is the name the driver is registered under with database/sql.
const DriverName = "hqq-sqlite"

// Compile-time checks of the database/sql interfaces the driver implements.
var (
	_ driver.Driver        = (*Driver)(nil)
	_ driver.DriverContext = (*Driver)(nil)
	_ driver.Connector     = (*Connector)(nil)
	_ io.Closer            = (*Connector)(nil)
)

func init() {
	sql.Register(DriverName, &Driver{})
}

// Driver is the database/sql driver "hqq-sqlite". It is registered by the
// package's init function.
type Driver struct{}

// Open connects to the service named by dsn and returns one connection. The
// connection owns the Connector it creates, so closing the connection closes
// the link. database/sql never calls Open: it uses OpenConnector.
func (d *Driver) Open(dsn string) (driver.Conn, error) {
	c, err := ConnectorFromDSN(dsn)
	if err != nil {
		return nil, err
	}
	dc, err := c.Connect(context.Background())
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	dc.(*conn).owner = c
	return dc, nil
}

// OpenConnector parses dsn and returns a Connector for it. database/sql uses
// it for sql.Open, so all connections of the pool share one link.
func (d *Driver) OpenConnector(dsn string) (driver.Connector, error) {
	c, err := ConnectorFromDSN(dsn)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ConnectorFromDSN parses dsn (see ParseDSN) and returns a Connector for it.
// Use it with sql.OpenDB to keep the Connector at hand, for example to Close
// the shared link after the pool is closed.
func ConnectorFromDSN(dsn string) (*Connector, error) {
	listener, cfg, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	return NewConnector(listener, cfg)
}

// Mode is the open mode of the database.
type Mode uint8

// Open modes. The zero value is ModeReadWriteCreate.
const (
	ModeReadWriteCreate Mode = iota // mode=rwc: open, creating the database if needed
	ModeReadWrite                   // mode=rw: open an existing database for writing
	ModeReadOnly                    // mode=ro: open read-only
)

// TxMode is the BEGIN mode of a transaction.
type TxMode = wire.TxMode

// Transaction modes. TxDeferred is the zero value.
const (
	TxDeferred  = wire.TxDeferred
	TxImmediate = wire.TxImmediate
	TxExclusive = wire.TxExclusive
)

// Pragma is a PRAGMA the server applies when a session opens.
type Pragma struct {
	Name  string
	Value string
}

// Config configures a Connector. The zero value is valid once Database is set.
type Config struct {
	// Database is the database alias or path, required.
	Database string

	// Mode is the open mode. Default: ModeReadWriteCreate.
	Mode Mode

	// Timeout bounds connecting (waiting for the listener, attaching the
	// link, and opening the session) and the requests that run outside a
	// caller context (Commit, Rollback, closing cursors and statements).
	// Default: 10s.
	Timeout time.Duration

	// FetchSize is the number of rows per batch. Default: 256; the server
	// caps it at wire.MaxFetchRows (4096).
	FetchSize int

	// BusyTimeout is how long the server waits for a lock held by another
	// session. Zero selects 5s; a negative value disables the wait.
	BusyTimeout time.Duration

	// TxMode is the BEGIN mode of transactions. A context value set with
	// WithTxMode overrides it per transaction. Default: TxDeferred.
	TxMode TxMode

	// Pragmas are applied by the server when a session opens.
	Pragmas []Pragma

	// Link configures the HQQ link. Zero fields select the driver's values
	// (64 buffers of 4096 bytes). Features and the message size are fixed by
	// the protocol and override any value given here; heartbeat and timeout
	// settings are used as given.
	Link hqq.Config
}

// Defaults of Config.
const (
	defaultTimeout     = 10 * time.Second
	defaultBusyTimeout = 5 * time.Second
	defaultFetchSize   = wire.DefaultFetchRows
	defaultBufferCount = 64
	defaultBufferSize  = 4096
)

// normalized returns cfg with the defaults applied and its values checked.
func (cfg Config) normalized() (Config, error) {
	if cfg.Database == "" || len(cfg.Database) > wire.MaxPathBytes {
		return cfg, fmt.Errorf("sqlite: database must be 1..%d bytes", wire.MaxPathBytes)
	}
	switch cfg.Mode {
	case ModeReadWriteCreate, ModeReadWrite, ModeReadOnly:
	default:
		return cfg, fmt.Errorf("sqlite: invalid mode %d", cfg.Mode)
	}
	switch cfg.TxMode {
	case TxDeferred, TxImmediate, TxExclusive:
	default:
		return cfg, fmt.Errorf("sqlite: invalid transaction mode %d", cfg.TxMode)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.FetchSize == 0 {
		cfg.FetchSize = defaultFetchSize
	}
	if cfg.FetchSize < 1 || cfg.FetchSize > wire.MaxFetchRows {
		return cfg, fmt.Errorf("sqlite: fetch size must be 1..%d", wire.MaxFetchRows)
	}
	if cfg.BusyTimeout == 0 {
		cfg.BusyTimeout = defaultBusyTimeout
	}
	for _, p := range cfg.Pragmas {
		if !wire.ValidPragmaName(p.Name) || !wire.ValidPragmaValue(p.Value) {
			return cfg, fmt.Errorf("sqlite: invalid pragma %q=%q", p.Name, p.Value)
		}
	}
	return cfg, nil
}

// linkConfig returns the HQQ configuration of the connector's link.
func (cfg Config) linkConfig() hqq.Config {
	lc := cfg.Link
	if lc.BufferCount == 0 {
		lc.BufferCount = defaultBufferCount
	}
	if lc.BufferSize == 0 {
		lc.BufferSize = defaultBufferSize
	}
	lc.Features = hqq.FeatureRequestResponse | hqq.FeatureCancel | hqq.FeatureMetadata
	lc.Advanced.MaxMessageSize = wire.MaxMessageBytes
	return lc
}

// ParseDSN parses a DSN of the form
//
//	hqq://<listener>?db=...&mode=...&timeout=...&fetch=...&busy=...&txlock=...&pragma.<name>=<value>
//
// and returns the listener name and the Config. Unknown options are errors, so
// misspelled options are not ignored. Every option may appear once.
func ParseDSN(dsn string) (string, Config, error) {
	var cfg Config
	u, err := url.Parse(dsn)
	if err != nil {
		return "", cfg, fmt.Errorf("sqlite: invalid DSN: %w", err)
	}
	if u.Scheme != "hqq" {
		return "", cfg, fmt.Errorf("sqlite: DSN scheme must be hqq, got %q", u.Scheme)
	}
	if u.Host == "" || u.Path != "" || u.User != nil {
		return "", cfg, errors.New("sqlite: DSN must be hqq://<listener>?<options>")
	}
	q := u.Query()
	var pragmas []Pragma
	for key, vals := range q {
		if !strings.HasPrefix(key, "pragma.") {
			continue
		}
		if len(vals) != 1 {
			return "", cfg, fmt.Errorf("sqlite: option %s must be given once", key)
		}
		pragmas = append(pragmas, Pragma{Name: strings.TrimPrefix(key, "pragma."), Value: vals[0]})
	}
	sort.Slice(pragmas, func(i, j int) bool { return pragmas[i].Name < pragmas[j].Name })
	cfg.Pragmas = pragmas
	for key, vals := range q {
		if strings.HasPrefix(key, "pragma.") {
			continue
		}
		if len(vals) != 1 {
			return "", cfg, fmt.Errorf("sqlite: option %s must be given once", key)
		}
		v := vals[0]
		switch key {
		case "db":
			cfg.Database = v
		case "mode":
			switch v {
			case "rwc", "":
				cfg.Mode = ModeReadWriteCreate
			case "rw":
				cfg.Mode = ModeReadWrite
			case "ro":
				cfg.Mode = ModeReadOnly
			default:
				return "", cfg, fmt.Errorf("sqlite: invalid mode %q", v)
			}
		case "timeout":
			cfg.Timeout, err = parseDuration(key, v)
		case "busy":
			cfg.BusyTimeout, err = parseDuration(key, v)
		case "fetch":
			cfg.FetchSize, err = strconv.Atoi(v)
			if err != nil {
				err = fmt.Errorf("sqlite: invalid fetch %q", v)
			}
		case "txlock":
			switch strings.ToLower(v) {
			case "deferred", "":
				cfg.TxMode = TxDeferred
			case "immediate":
				cfg.TxMode = TxImmediate
			case "exclusive":
				cfg.TxMode = TxExclusive
			default:
				return "", cfg, fmt.Errorf("sqlite: invalid txlock %q", v)
			}
		default:
			return "", cfg, fmt.Errorf("sqlite: unknown DSN option %q", key)
		}
		if err != nil {
			return "", cfg, err
		}
	}
	if cfg.Database == "" {
		return "", cfg, errors.New("sqlite: DSN option db is required")
	}
	return u.Host, cfg, nil
}

func parseDuration(key, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("sqlite: invalid %s %q: %w", key, v, err)
	}
	return d, nil
}

// Connector connects to one service. All connections of a Connector are
// streams of one shared link, which the Connector dials again when it dies.
// Connector implements io.Closer; closing it closes the link. sql.DB calls
// Close when the DB is closed, if the Connector implements io.Closer.
type Connector struct {
	listener string
	cfg      Config
	linkCfg  hqq.Config

	mu      sync.Mutex
	link    *hqq.NamedAdvancedLink
	dialing chan struct{} // closed when the dial in progress ends
	closed  bool
}

var errConnectorClosed = errors.New("sqlite: connector closed")

// NewConnector returns a Connector for the listener name and configuration.
func NewConnector(listener string, cfg Config) (*Connector, error) {
	if listener == "" {
		return nil, errors.New("sqlite: listener name is empty")
	}
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	return &Connector{listener: listener, cfg: cfg, linkCfg: cfg.linkConfig()}, nil
}

// Driver returns the driver of the connector.
func (c *Connector) Driver() driver.Driver { return &Driver{} }

// Close closes the shared link. Connections already open fail with
// driver.ErrBadConn; further Connect calls fail.
func (c *Connector) Close() error {
	c.mu.Lock()
	c.closed = true
	link := c.link
	c.link = nil
	c.mu.Unlock()
	if link != nil {
		return link.Close()
	}
	return nil
}

// Connect opens a session: it gets the shared link (dialing it if needed),
// opens a stream on it, and sends Open. ctx and Config.Timeout bound it.
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	link, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := link.OpenStream(ctx)
	if err != nil {
		return nil, c.linkError(link, err)
	}
	cn := &conn{c: c, link: link, st: stream, fetch: c.cfg.FetchSize}
	if err := cn.open(ctx); err != nil {
		cn.discard()
		return nil, err
	}
	return cn, nil
}

// acquire returns the shared link, dialing it if there is none or the one
// there is has died. Concurrent callers wait for a single dial.
func (c *Connector) acquire(ctx context.Context) (*hqq.NamedAdvancedLink, error) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, errConnectorClosed
		}
		if l := c.link; l != nil {
			if !linkDead(l) {
				c.mu.Unlock()
				return l, nil
			}
			c.link = nil
			_ = l.Close()
		}
		if c.dialing != nil {
			wait := c.dialing
			c.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		c.dialing = done
		c.mu.Unlock()

		l, err := c.dial(ctx)

		c.mu.Lock()
		c.dialing = nil
		close(done)
		if err == nil && c.closed {
			err = errConnectorClosed
		}
		if err == nil {
			c.link = l
		}
		c.mu.Unlock()
		if err != nil {
			if l != nil {
				_ = l.Close()
			}
			return nil, err
		}
		return l, nil
	}
}

// dial connects the link and checks that the service negotiated the features
// the protocol needs.
func (c *Connector) dial(ctx context.Context) (*hqq.NamedAdvancedLink, error) {
	l, err := hqq.Connect(ctx, c.listener, c.linkCfg)
	if err != nil {
		return nil, transportError(err)
	}
	const need = hqq.FeatureRequestResponse | hqq.FeatureCancel
	if l.NegotiatedFeatures()&need != need {
		_ = l.Close()
		return nil, fmt.Errorf("sqlite: service %q does not support request/response with cancel", c.listener)
	}
	return l, nil
}

// linkDead reports whether the peer of l is gone or closed the link.
func linkDead(l *hqq.NamedAdvancedLink) bool {
	st := l.PeerState()
	return st == hqq.PeerGone || st == hqq.PeerClosed
}

// linkError maps an error of a link operation. A lost link is dropped so the
// next Connect dials again.
func (c *Connector) linkError(link *hqq.NamedAdvancedLink, err error) error {
	if errors.Is(err, hqq.ErrPeerGone) || errors.Is(err, hqq.ErrPeerClosed) {
		c.dropLink(link)
	}
	return transportError(err)
}

// dropLink forgets link if it is still the shared one, and closes it.
func (c *Connector) dropLink(link *hqq.NamedAdvancedLink) {
	c.mu.Lock()
	if c.link == link {
		c.link = nil
	}
	c.mu.Unlock()
	_ = link.Close()
}

// transportError marks a failure of the link or of the peer as
// driver.ErrBadConn, so database/sql discards the connection and may retry on
// a new one. It is used where no statement can have run: dialing and opening
// a session.
func transportError(err error) error {
	if errors.Is(err, driver.ErrBadConn) {
		return err
	}
	if isTransportError(err) {
		return fmt.Errorf("%w: %w", driver.ErrBadConn, err)
	}
	return err
}

// isTransportError reports whether err is a failure of the link or the peer.
func isTransportError(err error) bool {
	return errors.Is(err, hqq.ErrPeerGone) || errors.Is(err, hqq.ErrPeerClosed) ||
		errors.Is(err, hqq.ErrClosed) || errors.Is(err, hqq.ErrListenerGone) ||
		errors.Is(err, hqq.ErrListenerClosed) || errors.Is(err, hqq.ErrShuttingDown)
}

type txModeKey struct{}

// WithTxMode returns a context whose transactions begin in mode, overriding
// Config.TxMode. The context is the one passed to BeginTx.
func WithTxMode(ctx context.Context, mode TxMode) context.Context {
	return context.WithValue(ctx, txModeKey{}, mode)
}

func txModeFrom(ctx context.Context, def TxMode) TxMode {
	if m, ok := ctx.Value(txModeKey{}).(TxMode); ok {
		return m
	}
	return def
}
