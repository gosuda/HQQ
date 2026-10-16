package main

import (
	"fmt"
	"time"

	"gosuda.org/hqq"
)

// advClients is the number of clients the opener connects, one after the other.
const advClients = 3

func init() {
	register(&scenario{
		ID: "X-A10",
		Title: "acceptor (LISTENER_DRAFT, Listen/Dial/Connect): the creator runs NewAcceptor under the name; a second " +
			"NewAcceptor on the same name fails with ErrListenerBusy; 3 clients (opener) Connect one after the other, " +
			"each link answers one request (echo) and is closed; the pending Accept of a fourth link fails with " +
			"ErrAcceptorClosed after Close. Creator is the acceptor (Secondary of every accepted link), opener the client " +
			"(Primary)",
		Kind:      "acceptor",
		Platforms: []string{"any"},
		TimeoutS:  30,
		Variants:  []variant{advVariant("default", advBase())},
		Run:       runAdvAcceptor,
	})
}

func runAdvAcceptor(e *env) error {
	if e.cfg.Role == "opener" {
		return advClient(e)
	}
	return advAcceptorSide(e)
}

// advAcceptorSide is the creator: the acceptor and the per-link servers.
func advAcceptorSide(e *env) error {
	acc, err := hqq.NewAcceptor(e.cfg.Name, hqq.AcceptorConfig{Advanced: advOptions(e.cfg)})
	if err != nil {
		return err
	}
	defer acc.Close()
	if err := e.barrier("listening"); err != nil {
		return err
	}

	second, err := hqq.NewAcceptor(e.cfg.Name, hqq.AcceptorConfig{Advanced: advOptions(e.cfg)})
	if second != nil {
		_ = second.Close()
	}
	e.checkErr("second acceptor on the same name is ErrListenerBusy", err, hqq.ErrListenerBusy)

	links := make([]*hqq.NamedAdvancedLink, 0, advClients)
	defer func() {
		for _, l := range links {
			_ = l.Close()
		}
	}()
	for i := range advClients {
		l, err := acc.Accept(e.ctx)
		if err != nil {
			return fmt.Errorf("accept %d: %w", i, err)
		}
		links = append(links, l)
		advServeAsync(e, l.AdvancedLink, advEchoMux())
	}
	e.res.Received = advClients
	if err := e.barrier("clients-done"); err != nil {
		return err
	}

	pending := make(chan error, 1)
	go func() {
		_, err := acc.Accept(e.ctx)
		pending <- err
	}()
	if err := acc.Close(); err != nil {
		return err
	}
	select {
	case err := <-pending:
		e.checkErr("pending Accept fails with ErrAcceptorClosed after Close", err, hqq.ErrAcceptorClosed)
	case <-time.After(5 * time.Second):
		e.check("pending Accept fails with ErrAcceptorClosed after Close", false, "Accept did not return")
	}
	return e.barrier("end")
}

// advClient is the opener: it connects advClients times, one request per link.
func advClient(e *env) error {
	if err := e.barrier("listening"); err != nil {
		return err
	}
	cfg := hqq.Config{BufferCount: e.cfg.BufferCount, BufferSize: e.cfg.BufferSize, Advanced: advOptions(e.cfg)}
	for i := range advClients {
		l, err := hqq.Connect(e.ctx, e.cfg.Name, cfg)
		if err != nil {
			return fmt.Errorf("connect %d: %w", i, err)
		}
		payload := fmt.Sprintf("client %d", i)
		m, err := l.Call(e.ctx, advMethodEcho, []byte(payload))
		if err != nil {
			_ = l.Close()
			return fmt.Errorf("call %d: %w", i, err)
		}
		e.check(fmt.Sprintf("client %d: the accepted link answers its request", i), string(m.Bytes()) == payload, string(m.Bytes()))
		m.Release()
		_ = l.Close()
	}
	e.res.Sent = advClients
	if err := e.barrier("clients-done"); err != nil {
		return err
	}
	return e.barrier("end")
}
