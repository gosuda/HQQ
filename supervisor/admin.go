package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"gosuda.org/hqq"
)

// Admin protocol: an HQQ Advanced link carries JSON requests to these method
// IDs. Requests and responses are JSON objects; errors are ResponseError
// values whose payload is {"error": "..."} and whose status is one of the
// Status codes below.
const (
	MethodList    uint64 = 1 // no payload -> ListResponse
	MethodStatus  uint64 = 2 // {"name"} -> Status
	MethodStart   uint64 = 3 // {"name"} -> Status
	MethodStop    uint64 = 4 // {"name"} -> Status
	MethodRestart uint64 = 5 // {"name"} -> Status
	MethodReload  uint64 = 6 // Config JSON, or no payload to reload the configured file -> ListResponse
)

// Error statuses of the admin protocol.
const (
	AdminStatusBadRequest uint64 = 0x101
	AdminStatusNotFound   uint64 = 0x102
	AdminStatusFailed     uint64 = 0x103
)

// ListResponse is the payload of MethodList and MethodReload.
type ListResponse struct {
	Services []Status `json:"services"`
}

// AdminError is an error reported by the supervisor over the admin protocol.
type AdminError struct {
	Status  uint64
	Message string
}

func (e *AdminError) Error() string { return e.Message }

type nameRequest struct {
	Name string `json:"name"`
}

type errorBody struct {
	Error string `json:"error"`
}

// ServeAdmin creates the HQQ listener name (permission perm) and serves the
// admin protocol on it for sv until ctx ends. reload supplies the
// configuration for MethodReload requests without a payload; it may be nil.
// The listener name is removed when ServeAdmin returns.
func ServeAdmin(ctx context.Context, sv *Supervisor, listener string, perm os.FileMode, reload func(context.Context) (Config, error)) error {
	acc, err := hqq.NewAcceptorContext(ctx, listener, hqq.AcceptorConfig{Perm: perm})
	if err != nil {
		return fmt.Errorf("admin listener %q: %w", listener, err)
	}
	defer acc.Close()
	mux := adminMux(sv, reload)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		link, err := acc.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer link.Close()
			_ = link.Serve(ctx, mux)
		}()
	}
}

func adminMux(sv *Supervisor, reload func(context.Context) (Config, error)) *hqq.Mux {
	m := hqq.NewMux()
	m.HandleFunc(MethodList, func(_ context.Context, w *hqq.ResponseWriter, _ *hqq.Request) {
		writeJSON(w, ListResponse{Services: sv.List()})
	})
	m.HandleFunc(MethodStatus, func(_ context.Context, w *hqq.ResponseWriter, req *hqq.Request) {
		name, ok := decodeName(w, req)
		if !ok {
			return
		}
		st, err := sv.Status(name)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, st)
	})
	m.Handle(MethodStart, nameOp(sv, (*Supervisor).Start))
	m.Handle(MethodStop, nameOp(sv, (*Supervisor).Stop))
	m.Handle(MethodRestart, nameOp(sv, (*Supervisor).Restart))
	m.HandleFunc(MethodReload, func(ctx context.Context, w *hqq.ResponseWriter, req *hqq.Request) {
		var cfg Config
		var err error
		if payload := bytes.TrimSpace(req.Payload()); len(payload) == 0 {
			if reload == nil {
				writeErrorStatus(w, AdminStatusBadRequest, errors.New("no configuration file to reload"))
				return
			}
			cfg, err = reload(ctx)
		} else {
			cfg, err = DecodeConfig(payload)
		}
		if err != nil {
			writeErrorStatus(w, AdminStatusBadRequest, err)
			return
		}
		if err := sv.Reload(ctx, cfg); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, ListResponse{Services: sv.List()})
	})
	return m
}

// nameOp adapts a Supervisor operation on a service name to a handler that
// answers with the service status.
func nameOp(sv *Supervisor, op func(*Supervisor, context.Context, string) error) hqq.HandlerFunc {
	return func(ctx context.Context, w *hqq.ResponseWriter, req *hqq.Request) {
		name, ok := decodeName(w, req)
		if !ok {
			return
		}
		if err := op(sv, ctx, name); err != nil {
			writeError(w, err)
			return
		}
		st, err := sv.Status(name)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, st)
	}
}

func decodeName(w *hqq.ResponseWriter, req *hqq.Request) (string, bool) {
	var r nameRequest
	if err := json.Unmarshal(req.Payload(), &r); err != nil || r.Name == "" {
		writeErrorStatus(w, AdminStatusBadRequest, errors.New(`request needs {"name": "<service>"}`))
		return "", false
	}
	return r.Name, true
}

func writeJSON(w *hqq.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeErrorStatus(w, AdminStatusFailed, err)
		return
	}
	_, _ = w.Write(b)
}

func writeError(w *hqq.ResponseWriter, err error) {
	status := AdminStatusFailed
	if errors.Is(err, ErrUnknownService) {
		status = AdminStatusNotFound
	}
	writeErrorStatus(w, status, err)
}

func writeErrorStatus(w *hqq.ResponseWriter, status uint64, err error) {
	b, _ := json.Marshal(errorBody{Error: err.Error()})
	w.WriteError(status, b)
}

// AdminClient is a connection to the admin listener of a supervisor.
type AdminClient struct {
	link *hqq.NamedAdvancedLink
}

// DialAdmin connects to the admin listener name. It fails when no supervisor
// serves it before ctx ends.
func DialAdmin(ctx context.Context, listener string) (*AdminClient, error) {
	link, err := hqq.Connect(ctx, listener, hqq.Config{})
	if err != nil {
		return nil, fmt.Errorf("connect to admin listener %q: %w", listener, err)
	}
	return &AdminClient{link: link}, nil
}

// Close disconnects.
func (c *AdminClient) Close() error {
	return c.link.Close()
}

func (c *AdminClient) call(ctx context.Context, method uint64, in []byte, out any) error {
	msg, err := c.link.Call(ctx, method, in)
	if err != nil {
		var re hqq.ResponseError
		if errors.As(err, &re) {
			return &AdminError{Status: re.Status, Message: errorText(re.Payload)}
		}
		return err
	}
	defer msg.Release()
	if out != nil {
		return json.Unmarshal(msg.Bytes(), out)
	}
	return nil
}

func errorText(p []byte) string {
	var b errorBody
	if json.Unmarshal(p, &b) == nil && b.Error != "" {
		return b.Error
	}
	return string(p)
}

// List returns the status of every service.
func (c *AdminClient) List(ctx context.Context) ([]Status, error) {
	var r ListResponse
	if err := c.call(ctx, MethodList, nil, &r); err != nil {
		return nil, err
	}
	return r.Services, nil
}

// Status returns the status of one service.
func (c *AdminClient) Status(ctx context.Context, name string) (Status, error) {
	return c.byName(ctx, MethodStatus, name)
}

// Start starts a service and returns its status.
func (c *AdminClient) Start(ctx context.Context, name string) (Status, error) {
	return c.byName(ctx, MethodStart, name)
}

// Stop stops a service and returns its status.
func (c *AdminClient) Stop(ctx context.Context, name string) (Status, error) {
	return c.byName(ctx, MethodStop, name)
}

// Restart restarts a service and returns its status.
func (c *AdminClient) Restart(ctx context.Context, name string) (Status, error) {
	return c.byName(ctx, MethodRestart, name)
}

func (c *AdminClient) byName(ctx context.Context, method uint64, name string) (Status, error) {
	in, err := json.Marshal(nameRequest{Name: name})
	if err != nil {
		return Status{}, err
	}
	var st Status
	if err := c.call(ctx, method, in, &st); err != nil {
		return Status{}, err
	}
	return st, nil
}

// Reload applies a configuration. cfg is the configuration JSON; nil asks the
// supervisor to reload its own configuration file. It returns the services
// after the reload.
func (c *AdminClient) Reload(ctx context.Context, cfg []byte) ([]Status, error) {
	var r ListResponse
	if err := c.call(ctx, MethodReload, cfg, &r); err != nil {
		return nil, err
	}
	return r.Services, nil
}
