package supervisor

import (
	"context"
	"errors"
	"time"

	"gosuda.org/hqq"
)

// checkHealth connects to the listener and, for ping != 0, calls the ping
// method. The link is closed again, which removes the client segment. The
// whole check is bounded by timeout.
func checkHealth(ctx context.Context, listener string, timeout time.Duration, ping uint64) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	link, err := hqq.Connect(ctx, listener, hqq.Config{})
	if err != nil {
		return err
	}
	defer link.Close()
	if ping == 0 {
		return nil
	}
	msg, err := link.Call(ctx, ping, nil)
	if err != nil {
		return err
	}
	msg.Release()
	return nil
}

// cleanStaleListener removes the listener name before a restart when no
// acceptor answers under it. A live acceptor is left alone; a name that never
// answers is removed only for the errors that mean "nobody serves it" (a
// foreign object of another layout is never touched).
func cleanStaleListener(spec ServiceSpec) {
	ctx, cancel := context.WithTimeout(context.Background(), spec.Health.timeout())
	defer cancel()
	link, err := hqq.Connect(ctx, spec.Listener, hqq.Config{})
	if err == nil {
		_ = link.Close()
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, hqq.ErrListenerGone) || errors.Is(err, hqq.ErrListenerClosed) {
		_ = hqq.Unlink(spec.Listener)
	}
}
