//go:build distributed || kubernetes

package testkit

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Images of the shared stores, pinned by digest. They are the versions the
// distributed suite verifies.
const (
	PostgresImage = "postgres:17.12-alpine@sha256:a77aa70e641402a21411b92b721c58d10f23b50ae5fdd2333efd22d166e1e5f7"
	RedisImage    = "redis:7.4.12-alpine@sha256:7f7d30b7fdd09467bac58198a1f166dd69789d348d4fe859cccb20e0cf287fbe"
)

// Databases is one Postgres and one Redis container shared by a test binary.
// Each test gets its own database and its own Redis logical database, so tests
// run in parallel without seeing each other's state.
type Databases struct {
	postgres  string
	redis     string
	pgAddr    string
	redisAddr string
	password  string

	mu      sync.Mutex
	redisDB int
}

var shared *Databases

// RunWithDatabases starts the containers, runs the tests and removes them.
// A missing Docker fails the binary: the distributed suite never skips
// silently.
func RunWithDatabases(m interface{ Run() int }) int {
	d, err := startDatabases()
	if err != nil {
		fmt.Fprintln(os.Stderr, "distributed test databases:", err)
		return 1
	}
	shared = d
	code := m.Run()
	d.stop()
	return code
}

func randomHex(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

func docker(args ...string) (string, error) {
	var out, errOut bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func hostPort(container, port string) (string, error) {
	out, err := docker("port", container, port)
	if err != nil {
		return "", err
	}
	line := strings.Split(out, "\n")[0]
	_, p, err := net.SplitHostPort(strings.TrimSpace(line))
	if err != nil {
		return "", err
	}
	return net.JoinHostPort("127.0.0.1", p), nil
}

// freePort reserves a loopback port for a container, so a restart keeps the
// address the clients already use.
func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	return port, err
}

func startDatabases() (*Databases, error) {
	d := &Databases{password: randomHex(16)}
	name := "comporta-test-" + randomHex(4)
	pgPort, err := freePort()
	if err != nil {
		return nil, err
	}
	redisPort, err := freePort()
	if err != nil {
		return nil, err
	}
	d.postgres, err = docker("run", "-d", "--rm", "--name", name+"-pg", "-e", "POSTGRES_PASSWORD="+d.password,
		"-p", "127.0.0.1:"+pgPort+":5432", PostgresImage, "-c", "max_connections=400", "-c", "fsync=on")
	if err != nil {
		return nil, err
	}
	// The scripts need every reservation to survive a restart: AOF with fsync on
	// every write and no eviction, as in production.
	d.redis, err = docker("run", "-d", "--name", name+"-redis", "-p", "127.0.0.1:"+redisPort+":6379", RedisImage,
		"redis-server", "--appendonly", "yes", "--appendfsync", "always", "--maxmemory-policy", "noeviction", "--databases", "64")
	if err != nil {
		d.stop()
		return nil, err
	}
	if d.pgAddr, err = hostPort(d.postgres, "5432"); err != nil {
		d.stop()
		return nil, err
	}
	if d.redisAddr, err = hostPort(d.redis, "6379"); err != nil {
		d.stop()
		return nil, err
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		db, err := sql.Open("pgx", d.adminDSN("postgres"))
		if err == nil {
			err = db.Ping()
			db.Close()
		}
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			d.stop()
			return nil, fmt.Errorf("postgres did not start")
		}
		time.Sleep(500 * time.Millisecond)
	}
	for {
		conn, err := net.DialTimeout("tcp", d.redisAddr, time.Second)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			d.stop()
			return nil, fmt.Errorf("redis did not start")
		}
		time.Sleep(200 * time.Millisecond)
	}
	return d, nil
}

func (d *Databases) stop() {
	for _, id := range []string{d.postgres, d.redis} {
		if id != "" {
			_, _ = docker("rm", "-f", id)
		}
	}
}

func (d *Databases) adminDSN(database string) string {
	return "postgres://postgres:" + d.password + "@" + d.pgAddr + "/" + database + "?sslmode=disable"
}

// Postgres creates an empty database for one test and returns its DSN. The
// address is loopback, where plain transport is allowed.
func Postgres(t testing.TB) string {
	t.Helper()
	if shared == nil {
		t.Fatal("RunWithDatabases was not called from TestMain")
	}
	name := "t_" + randomHex(8)
	db, err := sql.Open("pgx", shared.adminDSN("postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db, err := sql.Open("pgx", shared.adminDSN("postgres"))
		if err == nil {
			_, _ = db.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
			db.Close()
		}
	})
	return shared.adminDSN(name)
}

// Redis returns the address and a logical database of its own for one test.
func Redis(t testing.TB) (string, int) {
	t.Helper()
	if shared == nil {
		t.Fatal("RunWithDatabases was not called from TestMain")
	}
	shared.mu.Lock()
	shared.redisDB++
	db := shared.redisDB % 64
	shared.mu.Unlock()
	return shared.redisAddr, db
}

// RedisContainer is the container ID, for tests that restart Redis.
func RedisContainer() string { return shared.redis }

// RestartRedis restarts the Redis process. With AOF its data survives; the
// run identifier changes, which the coordinator must detect.
func RestartRedis(t testing.TB) {
	t.Helper()
	if _, err := docker("restart", "-t", "1", shared.redis); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		addr, err := hostPort(shared.redis, "6379")
		if err == nil {
			shared.redisAddr = addr
			if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
				conn.Close()
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("redis did not come back")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// PauseRedis freezes the Redis process, which looks like a network partition.
func PauseRedis(t testing.TB) func() {
	t.Helper()
	if _, err := docker("pause", shared.redis); err != nil {
		t.Fatal(err)
	}
	return func() { _, _ = docker("unpause", shared.redis) }
}

// WithContext is a deadline for one test step.
func WithContext(t testing.TB, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
