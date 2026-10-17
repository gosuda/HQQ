package contract

import (
	"database/sql"
	"fmt"
	"testing"
)

func testStatements(t *testing.T, db *sql.DB, caps Capabilities) {
	tbl := newTable(t, db, "stmt", "id INTEGER PRIMARY KEY, v TEXT")

	t.Run("prepared statements run with different arguments", func(t *testing.T) {
		ins, err := db.Prepare("INSERT INTO " + tbl + " (id, v) VALUES (?, ?)")
		must(t, err, "prepare insert")
		defer ins.Close()
		for i := 1; i <= 3; i++ {
			res, err := ins.Exec(i, fmt.Sprintf("v%d", i))
			must(t, err, "exec prepared insert")
			if n, _ := res.RowsAffected(); n != 1 {
				t.Fatalf("RowsAffected = %d, want 1", n)
			}
		}
		sel, err := db.Prepare("SELECT v FROM " + tbl + " WHERE id = ?")
		must(t, err, "prepare select")
		defer sel.Close()
		for i := 1; i <= 3; i++ {
			var v string
			must(t, sel.QueryRow(i).Scan(&v), "query prepared select")
			if v != fmt.Sprintf("v%d", i) {
				t.Fatalf("id %d: v = %q", i, v)
			}
		}
		rows, err := sel.Query(2)
		must(t, err, "Stmt.Query")
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		must(t, rows.Err(), "rows")
		if n != 1 {
			t.Fatalf("got %d rows, want 1", n)
		}
	})

	t.Run("NumInput is enforced", func(t *testing.T) {
		st, err := db.Prepare("INSERT INTO " + tbl + " (id, v) VALUES (?, ?)")
		must(t, err, "prepare")
		defer st.Close()
		if _, err := st.Exec(90); err == nil {
			t.Fatal("one argument for two parameters accepted")
		}
		// Exec without a prepared statement may skip database/sql's count check,
		// so the count is checked on prepared statements.
		two, err := db.Prepare("SELECT ?, ?")
		must(t, err, "prepare two-parameter select")
		defer two.Close()
		if rows, err := two.Query(1); err == nil {
			rows.Close()
			t.Fatal("one argument for two parameters accepted by Query")
		}
		none, err := db.Prepare("SELECT 1")
		must(t, err, "prepare select without parameters")
		defer none.Close()
		if rows, err := none.Query(1); err == nil {
			rows.Close()
			t.Fatal("an argument for a statement without parameters accepted")
		}
		wantInts(t, db, "SELECT id FROM "+tbl+" WHERE id = 90")
	})

	t.Run("Stmt.Close is idempotent", func(t *testing.T) {
		st, err := db.Prepare("SELECT v FROM " + tbl + " WHERE id = ?")
		must(t, err, "prepare")
		must(t, st.Close(), "first Close")
		if err := st.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		if rows, err := st.Query(1); err == nil {
			rows.Close()
			t.Fatal("Query on a closed statement succeeded")
		}
	})

	t.Run("statement runs after its connection was returned", func(t *testing.T) {
		ctx := testCtx(t)
		st, err := db.Prepare("SELECT v FROM " + tbl + " WHERE id = ?")
		must(t, err, "prepare")
		defer st.Close()
		var v string
		must(t, st.QueryRow(1).Scan(&v), "first use")
		// Hold a connection, so the next use may need another one.
		held, err := db.Conn(ctx)
		must(t, err, "conn")
		defer held.Close()
		must(t, st.QueryRow(2).Scan(&v), "use on another connection")
		if v != "v2" {
			t.Fatalf("v = %q, want v2", v)
		}
	})
}

func testResults(t *testing.T, db *sql.DB, caps Capabilities) {
	tbl := newTable(t, db, "res", "id INTEGER PRIMARY KEY, v TEXT")

	expect := func(t *testing.T, query string, affected, lastID int64) {
		t.Helper()
		res := exec(t, db, query)
		if n, err := res.RowsAffected(); err != nil || n != affected {
			t.Fatalf("%s: RowsAffected = %d, %v; want %d", query, n, err, affected)
		}
		if lastID < 0 {
			return
		}
		if id, err := res.LastInsertId(); err != nil || id != lastID {
			t.Fatalf("%s: LastInsertId = %d, %v; want %d", query, id, err, lastID)
		}
	}

	t.Run("insert takes the next rowid", func(t *testing.T) {
		expect(t, "INSERT INTO "+tbl+" (v) VALUES ('a')", 1, 1)
	})
	t.Run("explicit rowid", func(t *testing.T) {
		expect(t, "INSERT INTO "+tbl+" (id, v) VALUES (10, 'b')", 1, 10)
	})
	t.Run("rowid follows the explicit one", func(t *testing.T) {
		expect(t, "INSERT INTO "+tbl+" (v) VALUES ('c')", 1, 11)
	})
	t.Run("multi-row insert reports the last rowid", func(t *testing.T) {
		expect(t, "INSERT INTO "+tbl+" (v) VALUES ('d'), ('e'), ('f')", 3, 14)
	})
	t.Run("UPDATE rows affected", func(t *testing.T) {
		expect(t, "UPDATE "+tbl+" SET v = 'z' WHERE id < 12", 3, -1)
		expect(t, "UPDATE "+tbl+" SET v = 'z' WHERE id < 0", 0, -1)
	})
	t.Run("DELETE rows affected", func(t *testing.T) {
		expect(t, "DELETE FROM "+tbl+" WHERE id = 10", 1, -1)
		expect(t, "DELETE FROM "+tbl+" WHERE id > 100", 0, -1)
	})
}

func testLifecycle(t *testing.T, db *sql.DB, caps Capabilities) {
	tbl := newTable(t, db, "life", "id INTEGER PRIMARY KEY")
	exec(t, db, "INSERT INTO "+tbl+" (id) VALUES (1)")
	st, err := db.Prepare("SELECT id FROM " + tbl + " WHERE id = ?")
	must(t, err, "prepare")

	must(t, db.Close(), "Close")
	if _, err := db.Exec("INSERT INTO " + tbl + " (id) VALUES (2)"); err == nil {
		t.Fatal("Exec after Close succeeded")
	}
	if rows, err := db.Query("SELECT id FROM " + tbl); err == nil {
		rows.Close()
		t.Fatal("Query after Close succeeded")
	}
	if tx, err := db.Begin(); err == nil {
		_ = tx.Rollback()
		t.Fatal("Begin after Close succeeded")
	}
	if rows, err := st.Query(1); err == nil {
		rows.Close()
		t.Fatal("statement use after Close succeeded")
	}
}
