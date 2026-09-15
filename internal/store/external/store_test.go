package external

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

func TestOpenRejectsInsecureRemotePostgreSQL(t *testing.T) {
	_, err := Open(context.Background(), Config{
		DSN:       "postgresql://xirang:secret@db.example.test/xirang?sslmode=disable",
		Namespace: "production",
		CachePath: t.TempDir() + "/xirang.db",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot disable TLS") {
		t.Fatalf("insecure remote PostgreSQL DSN was not rejected: %v", err)
	}
}

func TestOpenRequiresAbsoluteCachePath(t *testing.T) {
	_, err := Open(context.Background(), Config{
		DSN:       "postgresql://localhost/xirang?sslmode=disable",
		Namespace: "production",
		CachePath: "relative/xirang.db",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "absolute cache path") {
		t.Fatalf("relative cache path was not rejected: %v", err)
	}
}

func TestExternalStateLeadershipAndTakeoverPreserveControlState(t *testing.T) {
	const driverName = "xirang-external-state-test"
	state := &fakePostgreSQL{}
	sql.Register(driverName, fakeDriver{state: state})
	config := Config{
		DSN: "postgresql://localhost/xirang?sslmode=disable", Namespace: "test",
		CachePath: t.TempDir() + "/active/xirang.db", driver: driverName,
	}
	active, err := Open(context.Background(), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := active.PutQueuePolicy(context.Background(), domain.QueuePolicy{Name: "priority", Weight: 7}); err != nil {
		t.Fatal(err)
	}
	conflictConfig := config
	conflictConfig.CachePath = t.TempDir() + "/conflict/xirang.db"
	if _, err := Open(context.Background(), conflictConfig, nil); err == nil || !strings.Contains(err.Error(), "leadership lock") {
		t.Fatalf("second active controller acquired leadership: %v", err)
	}
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}
	takeoverConfig := config
	takeoverConfig.CachePath = t.TempDir() + "/takeover/xirang.db"
	takeover, err := Open(context.Background(), takeoverConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	policies, err := takeover.ListQueuePolicies(context.Background())
	if err != nil || len(policies) != 1 || policies[0].Name != "priority" || policies[0].Weight != 7 {
		t.Fatalf("takeover did not restore external state: policies=%#v err=%v", policies, err)
	}
	if err := takeover.Close(); err != nil {
		t.Fatal(err)
	}
}

type fakePostgreSQL struct {
	mu                 sync.Mutex
	locked             bool
	image, imageSHA256 []byte
	namespace          string
	schemaVersion      int64
	updatedAt          time.Time
}

type fakeDriver struct{ state *fakePostgreSQL }

func (driver fakeDriver) Open(string) (driver.Conn, error) {
	return &fakeConnection{state: driver.state}, nil
}

type fakeConnection struct{ state *fakePostgreSQL }

func (connection *fakeConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not used")
}
func (connection *fakeConnection) Close() error { return nil }
func (connection *fakeConnection) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not used")
}
func (connection *fakeConnection) Ping(context.Context) error { return nil }

func (connection *fakeConnection) ExecContext(_ context.Context, query string, arguments []driver.NamedValue) (driver.Result, error) {
	connection.state.mu.Lock()
	defer connection.state.mu.Unlock()
	if strings.Contains(query, "INSERT INTO xirang_control_state") {
		connection.state.namespace = arguments[0].Value.(string)
		connection.state.schemaVersion = 1
		connection.state.image = append([]byte(nil), arguments[1].Value.([]byte)...)
		connection.state.imageSHA256 = append([]byte(nil), arguments[2].Value.([]byte)...)
		connection.state.updatedAt = time.Now().UTC()
	}
	return driver.RowsAffected(1), nil
}

func (connection *fakeConnection) QueryContext(_ context.Context, query string, arguments []driver.NamedValue) (driver.Rows, error) {
	connection.state.mu.Lock()
	defer connection.state.mu.Unlock()
	switch {
	case strings.Contains(query, "pg_try_advisory_lock"):
		acquired := !connection.state.locked
		if acquired {
			connection.state.locked = true
		}
		return &fakeRows{columns: []string{"pg_try_advisory_lock"}, values: [][]driver.Value{{acquired}}}, nil
	case strings.Contains(query, "pg_advisory_unlock"):
		unlocked := connection.state.locked
		connection.state.locked = false
		return &fakeRows{columns: []string{"pg_advisory_unlock"}, values: [][]driver.Value{{unlocked}}}, nil
	case strings.Contains(query, "pg_backend_pid"):
		return &fakeRows{columns: []string{"pg_backend_pid"}, values: [][]driver.Value{{int64(4242)}}}, nil
	case strings.Contains(query, "SELECT schema_version"):
		namespace := arguments[0].Value.(string)
		if connection.state.image == nil || namespace != connection.state.namespace {
			return &fakeRows{columns: []string{"schema_version", "image", "image_sha256"}}, nil
		}
		return &fakeRows{
			columns: []string{"schema_version", "image", "image_sha256"},
			values: [][]driver.Value{{
				connection.state.schemaVersion,
				append([]byte(nil), connection.state.image...),
				append([]byte(nil), connection.state.imageSHA256...),
			}},
		}, nil
	default:
		return nil, errors.New("unexpected query")
	}
}

type fakeRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (rows *fakeRows) Columns() []string { return rows.columns }
func (rows *fakeRows) Close() error      { return nil }
func (rows *fakeRows) Next(destination []driver.Value) error {
	if rows.index >= len(rows.values) {
		return io.EOF
	}
	copy(destination, rows.values[rows.index])
	rows.index++
	return nil
}

var _ driver.Driver = fakeDriver{}
var _ driver.Pinger = (*fakeConnection)(nil)
var _ driver.ExecerContext = (*fakeConnection)(nil)
var _ driver.QueryerContext = (*fakeConnection)(nil)
