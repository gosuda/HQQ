package hqq

import (
	"context"
	"errors"
	"os"
	"time"
)

// Default sizes used by Listen and Dial.
const (
	DefaultBufferCount = 256
	DefaultBufferSize  = 4096
)

// Config configures Listen and Dial. The zero value selects the defaults.
type Config struct {
	// BufferCount is the number of slots per direction (power of two).
	// Default: DefaultBufferCount.
	BufferCount int

	// BufferSize is the payload size of one slot (multiple of 8). Larger
	// messages are chunked automatically. Default: DefaultBufferSize.
	BufferSize int

	// Perm is the permission of the created segment (Listen only).
	// Default: 0600.
	Perm os.FileMode

	// Features to negotiate. Default: FeatureLargeCopy|FeatureRequestResponse.
	Features uint64

	// Advanced configures the link.
	Advanced AdvancedOptions
}

func (c Config) normalized() Config {
	if c.BufferCount == 0 {
		c.BufferCount = DefaultBufferCount
	}
	if c.BufferSize == 0 {
		c.BufferSize = DefaultBufferSize
	}
	if c.Perm == 0 {
		c.Perm = 0o600
	}
	if c.Features == FeatureNone {
		c.Features = supportedFeatures
	}
	return c
}

// Listen creates the named segment, waits until a peer Dials it, and
// negotiates the Advanced Protocol. The returned link is ready for Call,
// Serve, SendLarge and Receive. Closing it removes the name, unless the name
// was removed and created again by somebody else meanwhile.
//
// If the name already exists, Listen reuses it when both endpoints of the
// existing segment are closed or dead (for example after a crash of the
// previous listener) and fails with ErrLinkBusy when an endpoint is still
// alive; see CreateStandardLink. A name that is not an HQQ segment of this
// layout version fails with ErrIncompatibleLayout and is left untouched.
// ctx bounds the wait for a peer and the wait for a segment that another
// endpoint is still initializing.
func Listen(ctx context.Context, name string, cfg Config) (*NamedAdvancedLink, error) {
	ctx = orBackground(ctx)
	cfg = cfg.normalized()
	link, err := openNamedAdvanced(ctx, name, cfg.BufferCount, cfg.BufferSize, true, cfg.Perm, cfg.Advanced)
	if err != nil {
		return nil, err
	}
	if err := link.Negotiate(ctx, cfg.Features); err != nil {
		_ = link.Close()
		return nil, err
	}
	return link, nil
}

// Dial attaches to the named segment created by Listen as the Secondary and
// negotiates the Advanced Protocol. It retries while the name does not exist
// yet, while the listener has not finished initializing the segment, and
// while the segment has no live endpoint (until a new Listen reuses the
// name); every other error is returned at once. cfg must use the same
// BufferCount and BufferSize as the listener. ctx also bounds every wait
// inside a single attempt: Dial returns ctx.Err() as soon as ctx is done.
func Dial(ctx context.Context, name string, cfg Config) (*NamedAdvancedLink, error) {
	ctx = orBackground(ctx)
	cfg = cfg.normalized()
	backoff := time.Millisecond
	for {
		link, err := openNamedAdvanced(ctx, name, cfg.BufferCount, cfg.BufferSize, false, 0, cfg.Advanced)
		if err == nil {
			if err := link.Negotiate(ctx, cfg.Features); err != nil {
				_ = link.Close()
				return nil, err
			}
			return link, nil
		}
		if !dialRetryable(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 100*time.Millisecond)
	}
}

// dialRetryable reports whether a failed open may succeed later: the
// listener has not created the name yet, has not claimed or finished
// initializing the segment yet, or is gone and a new listener may reuse the
// name. Every other error (a busy link, a geometry or layout mismatch, an
// invalid name) is permanent.
func dialRetryable(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, errSegmentNotReady) ||
		errors.Is(err, errInitContended) ||
		errors.Is(err, errNoLiveEndpoint)
}
