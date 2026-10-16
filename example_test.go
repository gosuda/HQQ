package hqq_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"gosuda.org/hqq"
)

// shortName returns a per-process shared-memory name that fits the 31-byte
// macOS limit.
func shortName(tag string) string {
	return fmt.Sprintf("/hqq-%s-%d", tag, os.Getpid()%1000000)
}

// Listen and Dial connect two processes (here: two goroutines) over a named
// shared-memory segment and negotiate the Advanced Protocol; Mux and Serve
// turn the link into a small RPC server.
func ExampleListen() {
	name := shortName("rpc")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverDone := make(chan error, 1)
	go func() { // server process
		server, err := hqq.Listen(ctx, name, hqq.Config{})
		if err != nil {
			serverDone <- err
			return
		}
		defer server.Close()
		mux := hqq.NewMux()
		mux.HandleFunc(1, func(_ context.Context, req hqq.AdvancedMessage) ([]byte, error) {
			return append([]byte("hello, "), req.Payload...), nil
		})
		serverDone <- server.Serve(ctx, mux) // returns nil when the client closes
	}()

	// client process
	client, err := hqq.Dial(ctx, name, hqq.Config{})
	if err != nil {
		fmt.Println("dial:", err)
		return
	}
	resp, err := client.Call(ctx, 1, []byte("hqq"))
	if err != nil {
		fmt.Println("call:", err)
		return
	}
	fmt.Println(string(resp))
	client.Close()
	fmt.Println("server:", <-serverDone)
	// Output:
	// hello, hqq
	// server: <nil>
}

// NewStandardLink runs a link in any page-aligned memory. AlignedBuffer is
// enough between goroutines of one process; between processes use a shared
// mapping (CreateStandardLink / OpenStandardLink).
func ExampleNewStandardLink() {
	const bufferCount, bufferSize = 16, 256
	mem := hqq.AlignedBuffer(hqq.SizeStandardLink(bufferCount, bufferSize))

	a, err := hqq.NewStandardLink(mem, bufferCount, bufferSize, hqq.LinkOptions{})
	if err != nil {
		panic(err)
	}
	defer a.Close()
	b, err := hqq.NewStandardLink(mem, bufferCount, bufferSize, hqq.LinkOptions{})
	if err != nil {
		panic(err)
	}
	defer b.Close()

	a.Write([]byte("ping"))
	buf := make([]byte, bufferSize)
	n, _ := b.Read(buf)
	fmt.Println(string(buf[:n]), a.Mode(), b.Mode())
	// Output: ping primary secondary
}

// CreateStandardLink and OpenStandardLink map a named segment; the
// creator removes the name when it closes.
func ExampleCreateStandardLink() {
	name := shortName("std")
	server, err := hqq.CreateStandardLink(name, 64, 1024, 0o600, hqq.LinkOptions{})
	if err != nil {
		panic(err)
	}
	defer server.Close()

	client, err := hqq.OpenStandardLink(name, 64, 1024, hqq.LinkOptions{})
	if err != nil {
		panic(err)
	}
	client.Write([]byte("from client"))
	client.Close()

	buf := make([]byte, 1024)
	n, _ := server.Read(buf)
	fmt.Println(string(buf[:n]))
	_, err = server.Read(buf)
	fmt.Println(err == io.EOF)
	// Output:
	// from client
	// true
}

// Reservations hand out the shared slot buffer for explicit, zero-copy
// filling. Every reservation must be committed (or aborted) promptly.
func ExampleStandardLink_ReserveWrite() {
	mem := hqq.AlignedBuffer(hqq.SizeStandardLink(8, 64))
	w, _ := hqq.NewStandardLink(mem, 8, 64, hqq.LinkOptions{})
	r, _ := hqq.NewStandardLink(mem, 8, 64, hqq.LinkOptions{})
	defer w.Close()
	defer r.Close()

	res, err := w.ReserveWrite()
	if err != nil {
		panic(err)
	}
	n := copy(res.Buffer(), "built in place")
	if err := res.Commit(n); err != nil {
		panic(err)
	}

	in, _ := r.ReserveRead()
	fmt.Println(string(in.Buffer()))
	in.Release()
	// Output: built in place
}
