// Command hqqd supervises processes that serve HQQ listeners: it starts them,
// restarts them with backoff, checks their health over HQQ, stops them
// gracefully and exposes a small admin protocol that hqqd ctl speaks.
//
//	hqqd [run] [-config hqqd.json]     run the supervisor (SIGINT/SIGTERM stop it, SIGHUP reloads the file)
//	hqqd check [-config hqqd.json]     validate a configuration file
//	hqqd ctl [-listener name] [-config hqqd.json] [-timeout 30s] <command> [args]
//	                                   list | status <svc> | start <svc> | stop <svc> | restart <svc> | reload [file]
//
// The configuration format is documented in cmd/hqqd/README.md.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gosuda.org/hqq/supervisor"
)

const (
	defaultAdminListener = "hqqd"
	defaultAdminPerm     = "0600"
	defaultConfigFile    = "hqqd.json"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "hqqd:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	command := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	switch command {
	case "run":
		return runCommand(args, stderr)
	case "check":
		return checkCommand(args, stdout)
	case "ctl":
		return ctlCommand(args, stdout, stderr)
	case "help":
		usage(stdout)
		return nil
	}
	return fmt.Errorf("unknown command %q (try: hqqd help)", command)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage:
  hqqd [run] [-config hqqd.json]
  hqqd check [-config hqqd.json]
  hqqd ctl [-listener name] [-config hqqd.json] [-timeout 30s] <command> [args]
    commands: list | status <svc> | start <svc> | stop <svc> | restart <svc> | reload [file]
`)
}

// fileConfig is the configuration file: the admin block, then the services.
type fileConfig struct {
	Admin adminConfig `json:"admin"`
	supervisor.Config
}

// adminConfig configures the admin listener.
type adminConfig struct {
	// Listener is the HQQ listener name of the admin protocol. Default: hqqd.
	Listener string `json:"listener,omitempty"`
	// Perm is the octal permission of the listener, as a string. Default: 0600.
	Perm string `json:"perm,omitempty"`
}

func (a adminConfig) name() string {
	if a.Listener == "" {
		return defaultAdminListener
	}
	return a.Listener
}

func (a adminConfig) perm() (os.FileMode, error) {
	p := a.Perm
	if p == "" {
		p = defaultAdminPerm
	}
	v, err := strconv.ParseUint(p, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("admin.perm %q: want an octal mode such as \"0600\"", a.Perm)
	}
	return os.FileMode(v), nil
}

// loadFile reads and strictly decodes a configuration file and validates it,
// including that the admin listener is not a service listener.
func loadFile(path string) (fileConfig, error) {
	var fc fileConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return fc, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fc); err != nil {
		return fc, fmt.Errorf("%s: %w", path, err)
	}
	if dec.More() {
		return fc, fmt.Errorf("%s: trailing data after the JSON object", path)
	}
	if err := fc.Config.Validate(); err != nil {
		return fc, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := fc.Admin.perm(); err != nil {
		return fc, fmt.Errorf("%s: %w", path, err)
	}
	for _, s := range fc.Services {
		if s.Listener != "" && s.Listener == fc.Admin.name() {
			return fc, fmt.Errorf("%s: service %q uses the admin listener %q", path, s.Name, s.Listener)
		}
	}
	return fc, nil
}

func checkCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("hqqd check", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigFile, "configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fc, err := loadFile(*configPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: OK (%d services, admin listener %q)\n", *configPath, len(fc.Services), fc.Admin.name())
	return nil
}

func runCommand(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("hqqd run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigFile, "configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	logger := log.New(stderr, "hqqd: ", log.LstdFlags)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// SIGHUP reloads the configuration file, like the reload command.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	return runSupervisor(ctx, *configPath, hup, logger)
}

// runSupervisor runs the services of the configuration file and serves the
// admin protocol until ctx ends, then stops the services gracefully. A value
// on hup (may be nil) reloads the file. It returns the admin listener error
// if the listener cannot be created.
func runSupervisor(ctx context.Context, path string, hup <-chan os.Signal, logger *log.Logger) error {
	fc, err := loadFile(path)
	if err != nil {
		return err
	}
	perm, err := fc.Admin.perm()
	if err != nil {
		return err
	}
	sv, err := supervisor.New(fc.Config, supervisor.WithEvents(func(ev supervisor.Event) {
		if ev.Message != "" {
			logger.Printf("%s: %s: %s", ev.Service, ev.State, ev.Message)
		} else {
			logger.Printf("%s: %s", ev.Service, ev.State)
		}
	}))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reload := func(context.Context) (supervisor.Config, error) {
		fc, err := loadFile(path)
		return fc.Config, err
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				fc, err := loadFile(path)
				if err == nil {
					err = sv.Reload(ctx, fc.Config)
				}
				if err != nil {
					logger.Printf("reload: %v", err)
				} else {
					logger.Printf("configuration reloaded from %s", path)
				}
			}
		}
	}()
	adminDone := make(chan error, 1)
	go func() {
		err := supervisor.ServeAdmin(ctx, sv, fc.Admin.name(), perm, reload)
		if err != nil {
			logger.Printf("admin listener %q: %v", fc.Admin.name(), err)
			cancel()
		}
		adminDone <- err
	}()
	logger.Printf("supervising %d service(s), admin listener %q", len(fc.Services), fc.Admin.name())
	runErr := sv.Run(ctx)
	cancel()
	adminErr := <-adminDone
	logger.Printf("stopped")
	if adminErr != nil {
		return adminErr
	}
	return runErr
}

func ctlCommand(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("hqqd ctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listener := fs.String("listener", defaultAdminListener, "admin listener name")
	configPath := fs.String("config", "", "read the admin listener name from this configuration file")
	timeout := fs.Duration("timeout", 30*time.Second, "how long a command may take")
	if err := fs.Parse(args); err != nil {
		return err
	}
	name := *listener
	seenListener := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "listener" {
			seenListener = true
		}
	})
	if *configPath != "" && !seenListener {
		fc, err := loadFile(*configPath)
		if err != nil {
			return err
		}
		name = fc.Admin.name()
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return errors.New("ctl needs a command: list, status, start, stop, restart or reload")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	cli, err := supervisor.DialAdmin(ctx, name)
	if err != nil {
		return err
	}
	defer cli.Close()

	var out any
	switch cmd := rest[0]; cmd {
	case "list":
		if len(rest) != 1 {
			return errors.New("usage: hqqd ctl list")
		}
		out, err = cli.List(ctx)
	case "status", "start", "stop", "restart":
		if len(rest) != 2 {
			return fmt.Errorf("usage: hqqd ctl %s <service>", cmd)
		}
		out, err = byName(ctx, cli, cmd, rest[1])
	case "reload":
		var payload []byte
		if len(rest) > 2 {
			return errors.New("usage: hqqd ctl reload [file]")
		}
		if len(rest) == 2 {
			fc, lerr := loadFile(rest[1])
			if lerr != nil {
				return lerr
			}
			if payload, err = json.Marshal(fc.Config); err != nil {
				return err
			}
		}
		out, err = cli.Reload(ctx, payload)
	default:
		return fmt.Errorf("unknown ctl command %q", cmd)
	}
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func byName(ctx context.Context, cli *supervisor.AdminClient, cmd, name string) (supervisor.Status, error) {
	switch cmd {
	case "start":
		return cli.Start(ctx, name)
	case "stop":
		return cli.Stop(ctx, name)
	case "restart":
		return cli.Restart(ctx, name)
	}
	return cli.Status(ctx, name)
}
