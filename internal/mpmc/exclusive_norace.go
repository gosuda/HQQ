//go:build !race

package mpmc

// exclusiveGuard detects concurrent use of one side of an exclusive ring in
// race builds. Here it is empty and its methods compile away.
type exclusiveGuard struct{}

func (*exclusiveGuard) enterProducer() {}
func (*exclusiveGuard) exitProducer()  {}
func (*exclusiveGuard) enterConsumer() {}
func (*exclusiveGuard) exitConsumer()  {}
