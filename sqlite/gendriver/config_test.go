package gendriver_test

import (
	"fmt"
	"testing"
	"time"

	"gosuda.org/hqq/sqlite/gendriver"
	"gosuda.org/hqq/sqlite/internal/wire"
)

func TestParseDSN(t *testing.T) {
	listener, cfg, err := gendriver.ParseDSN("hqq://svc?db=/p/q.db&mode=ro&timeout=2s&fetch=10&busy=-1s&txlock=exclusive&pragma.journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	if listener != "svc" || cfg.Database != "/p/q.db" || cfg.Mode != gendriver.ModeReadOnly || cfg.Timeout != 2*time.Second ||
		cfg.FetchSize != 10 || cfg.BusyTimeout != -time.Second || cfg.TxMode != gendriver.TxExclusive ||
		fmt.Sprint(cfg.Pragmas) != fmt.Sprint([]gendriver.Pragma{{Name: "journal_mode", Value: "WAL"}}) {
		t.Fatalf("listener %q, cfg %+v", listener, cfg)
	}
	for _, dsn := range []string{"postgres://svc?db=x", "hqq://?db=x", "hqq://svc", "hqq://svc?db=x&bogus=1", "hqq://svc?db=a&db=b"} {
		if _, _, err := gendriver.ParseDSN(dsn); err == nil {
			t.Errorf("%s accepted", dsn)
		}
	}
}

func TestNewConnectorValidation(t *testing.T) {
	bad := []gendriver.Config{
		{},
		{Database: "x", FetchSize: wire.MaxFetchRows + 1},
		{Database: "x", FetchSize: -1},
		{Database: "x", Mode: 9},
		{Database: "x", TxMode: 9},
		{Database: "x", Pragmas: []gendriver.Pragma{{Name: "bad name", Value: "1"}}},
	}
	for i, cfg := range bad {
		if _, err := gendriver.NewConnector("svc", cfg); err == nil {
			t.Errorf("config %d accepted", i)
		}
	}
	if _, err := gendriver.NewConnector("", gendriver.Config{Database: "x"}); err == nil {
		t.Error("empty listener accepted")
	}
}
