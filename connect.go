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

	// Mode is the permission of the created segment (Listen only).
	// Default: 0600.
	Mode os.FileMode

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
	if c.Mode == 0 {
		c.Mode = 0o600
	}
	if c.Features == FeatureNone {
		c.Features = supportedFeatures
	}
	return c
}

// Listen creates the named segment, waits until a peer Dials it, and
// negotiates the Advanced Protocol. The returned link is ready for Call,
// Serve, SendLarge and Receive. Closing it removes the name.
//
// Listen fails with an error matching os.ErrExist if the name is in use; a
// name left behind by a crashed process can be removed with Unlink.
func Listen(ctx context.Context, name string, cfg Config) (*NamedAdvancedLink, error) {
	ctx = orBackground(ctx)
	cfg = cfg.normalized()
	link, err := CreateAdvancedLinkWithOptions(name, cfg.BufferCount, cfg.BufferSize, cfg.Mode, cfg.Advanced)
	if err != nil {
		return nil, err
	}
	if err := link.Negotiate(ctx, cfg.Features); err != nil {
		_ = link.Close()
		return nil, err
	}
	return link, nil
}

// Dial attaches to the named segment created by Listen, retrying until it
// exists or ctx is done, and negotiates the Advanced Protocol. cfg must use
// the same BufferCount and BufferSize as the listener.
func Dial(ctx context.Context, name string, cfg Config) (*NamedAdvancedLink, error) {
	ctx = orBackground(ctx)
	cfg = cfg.normalized()
	backoff := time.Millisecond
	for {
		link, err := OpenNamedAdvancedLinkWithOptions(name, cfg.BufferCount, cfg.BufferSize, cfg.Advanced)
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
// listener has not created the name yet, has not sized the segment yet, or
// is still initializing it. Every other error (a busy link, a geometry or
// layout mismatch, an invalid name) is permanent.
func dialRetryable(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, errSegmentNotReady) ||
		errors.Is(err, errInitContended)
}
