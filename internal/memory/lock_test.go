package memory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const lockProbeEnv = "KENAZ_MEMORY_TEST_LOCK_PROBE"

// TestHelperLockProbe is the second process of
// TestOpenKeepsAnotherStoresLock: it reports whether it could take the
// write lock right now.
func TestHelperLockProbe(t *testing.T) {
	path := os.Getenv(lockProbeEnv)
	if path == "" {
		t.Skip("helper process only")
	}
	db, err := sql.Open("sqlite", dsn(path, false, 100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		fmt.Println("LOCK-BUSY")
		return
	}
	fmt.Println("LOCK-TAKEN")
	_, _ = c.ExecContext(context.Background(), "ROLLBACK")
}

// Opening a second Store in this process must not release the write lock
// another Store's transaction holds. Closing any descriptor of a file
// drops all of the process's POSIX locks on it, so Open must never open
// and close the database file itself.
func TestOpenKeepsAnotherStoresLock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "memory", "memory.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	opened := make(chan struct{})
	go func() { // its schema step waits for the lock: let it finish after the rollback
		defer close(opened)
		if s2, err := Open(path); err == nil {
			s2.Close()
		}
	}()
	time.Sleep(300 * time.Millisecond)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperLockProbe$", "-test.count=1")
	cmd.Env = append(os.Environ(), lockProbeEnv+"="+path)
	out, err := cmd.CombinedOutput()
	if _, rerr := c.ExecContext(ctx, "ROLLBACK"); rerr != nil {
		t.Fatal(rerr)
	}
	<-opened
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "LOCK-BUSY") {
		t.Fatalf("another process took the write lock while this one held it:\n%s", out)
	}
}
