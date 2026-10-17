package launcher

import (
	"context"
	"database/sql"
	"errors"

	"gosuda.org/hqq/sqlite"
)

// OpenOption configures the database connection made by Open and OpenManaged.
type OpenOption func(*openConfig)

type openConfig struct {
	sqlite   sqlite.Config
	database string
}

// WithDatabase sets the database to open: an alias of the server (see
// Config.Aliases) or a path under Config.AllowDir. It is required unless the
// sqlite.Config given to WithSQLiteConfig names one.
func WithDatabase(db string) OpenOption {
	return func(c *openConfig) { c.database = db }
}

// WithSQLiteConfig sets the driver configuration (timeouts, mode, pragmas,
// link options). Its Database is used unless WithDatabase is given.
func WithSQLiteConfig(cfg sqlite.Config) OpenOption {
	return func(c *openConfig) { c.sqlite = cfg }
}

// ErrNoDatabase is returned by Open when no database was named.
var ErrNoDatabase = errors.New("launcher: no database: pass WithDatabase")

// Open starts the server with Start and returns a *sql.DB on its listener for
// the database named by WithDatabase. Close the DB first, then the Server: closing
// the DB closes the shared link, and Server.Close stops the process. For both
// in one call, use OpenManaged.
//
// The database is opened lazily by the driver, so Open does not check that the
// database exists or can be opened; the first query does.
func Open(ctx context.Context, cfg Config, opts ...OpenOption) (*sql.DB, *Server, error) {
	var oc openConfig
	for _, o := range opts {
		if o != nil {
			o(&oc)
		}
	}
	if oc.database != "" {
		oc.sqlite.Database = oc.database
	}
	if oc.sqlite.Database == "" {
		return nil, nil, ErrNoDatabase
	}
	srv, err := Start(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	connector, err := sqlite.NewConnector(srv.Listener(), oc.sqlite)
	if err != nil {
		_ = srv.Close()
		return nil, nil, err
	}
	return sql.OpenDB(connector), srv, nil
}

// Managed is a database and the server it uses, as returned by OpenManaged.
type Managed struct {
	DB     *sql.DB
	Server *Server
}

// OpenManaged is Open returning both in one value. Close releases them.
func OpenManaged(ctx context.Context, cfg Config, opts ...OpenOption) (*Managed, error) {
	db, srv, err := Open(ctx, cfg, opts...)
	if err != nil {
		return nil, err
	}
	return &Managed{DB: db, Server: srv}, nil
}

// Close closes the database, then stops the server. It returns the first error:
// the DB's, else the server's exit status error.
func (m *Managed) Close() error {
	err := m.DB.Close()
	if serr := m.Server.Close(); err == nil {
		err = serr
	}
	return err
}
