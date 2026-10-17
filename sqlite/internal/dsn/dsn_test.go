package dsn

import (
	"fmt"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	o, err := Parse("hqq://svc?db=/p/q.db&mode=ro&timeout=2s&fetch=10&busy=-1s&txlock=EXCLUSIVE&pragma.synchronous=NORMAL&pragma.journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	want := Options{
		Listener: "svc", Database: "/p/q.db", Mode: ModeReadOnly, Timeout: 2 * time.Second, Fetch: 10,
		Busy: -time.Second, TxLock: TxExclusive,
		Pragmas: []Pragma{{"journal_mode", "WAL"}, {"synchronous", "NORMAL"}},
	}
	if fmt.Sprint(o) != fmt.Sprint(want) {
		t.Fatalf("got %+v, want %+v", o, want)
	}
	o, err = Parse("hqq://svc?db=main")
	if err != nil || o.Database != "main" || o.Mode != ModeReadWriteCreate || o.TxLock != TxDeferred {
		t.Fatalf("defaults: %+v, %v", o, err)
	}
}

func TestParseErrors(t *testing.T) {
	for _, s := range []string{
		"postgres://svc?db=x", "hqq://?db=x", "hqq://svc", "hqq://svc?db=x&bogus=1", "hqq://svc?db=x&fetch=many",
		"hqq://svc?db=x&mode=nope", "hqq://svc?db=x&timeout=soon", "hqq://svc?db=x&txlock=sometimes",
		"hqq://svc/path?db=x", "hqq://svc?db=a&db=b", "hqq://u@svc?db=x", "hqq://svc?db=x&pragma.a=1&pragma.a=2",
		"::bad",
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("%s accepted", s)
		}
	}
}
