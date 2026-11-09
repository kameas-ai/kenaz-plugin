package memory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const writerEnv = "KENAZ_MEMORY_TEST_WRITER"

// toleratedBusy reports an error this test accepts from anything but a save: on
// Windows, on a slow runner, a reader or the sync bookkeeping can outlast the busy wait
// and fail with SQLITE_BUSY. A save takes its turn (beginTx), so it never does.
func toleratedBusy(err error) bool {
	return err != nil && runtime.GOOS == "windows" && (strings.Contains(err.Error(), "SQLITE_BUSY") || strings.Contains(err.Error(), "database is locked"))
}

// TestHelperWriter is the second process of TestConcurrentProcesses: it
// writes into the store named by the environment, like the relay's
// approval executors do while an MCP server writes too.
func TestHelperWriter(t *testing.T) {
	path := os.Getenv(writerEnv)
	if path == "" {
		t.Skip("helper process only")
	}
	s, err := OpenProfile(path, acct)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	kept := 0
	for i := 0; i < 40; i++ {
		r, err := s.SaveNote(ctx, NewNote{ProjectKey: GlobalProject, Title: fmt.Sprint("child ", i), Body: "from the other process", Source: SourceCLI},
			SaveOptions{RequestKey: fmt.Sprint("child-", i), SyncApproved: true})
		if err != nil {
			t.Fatal(err)
		}
		kept++
		if i%4 == 0 {
			_, err := s.ForgetNote(ctx, r.ID, MutateOptions{RequestKey: fmt.Sprint("child-forget-", i), SyncApproved: true})
			if toleratedBusy(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			kept--
		}
	}
	fmt.Printf("KEPT=%d\n", kept)
}

// Two processes (an MCP server and the relay daemon) and several
// connections write one store at the same time: every write lands, none
// fails with "database is locked", and readers keep working.
func TestConcurrentProcesses(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kenaz state", DirName, "profiles", "x", FileName)
	s, err := OpenProfile(path, acct)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.EnableSync(ctx, "memory-sync/v1", MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperWriter$", "-test.count=1")
	cmd.Env = append(os.Environ(), writerEnv+"="+path)
	done := make(chan error, 1)
	var out []byte
	go func() {
		var err error
		out, err = cmd.CombinedOutput()
		done <- err
	}()
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for w := 0; w < 3; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := OpenProfile(path, acct)
			if err != nil {
				errs <- err
				return
			}
			defer st.Close()
			for i := 0; i < 20; i++ {
				if _, err := st.Save(ctx, NewNote{ProjectKey: GlobalProject, Title: fmt.Sprint("w", w, " ", i), Body: "parent", Source: SourceCLI}); err != nil {
					errs <- err
				}
				ro, err := OpenProfileReadOnly(path, acct, time.Second)
				if toleratedBusy(err) {
					continue
				}
				if err != nil {
					errs <- err
					continue
				}
				if _, err := ro.Count(ctx); err != nil && !toleratedBusy(err) {
					errs <- err
				}
				ro.Close()
			}
		}()
	}
	// The sync worker's lease and push bookkeeping run at the same time.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			l, err := s.AcquireLease(ctx, "relay", time.Minute)
			if toleratedBusy(err) {
				continue
			}
			if err != nil {
				errs <- err
				return
			}
			b, err := s.LeasePushBatch(ctx, l)
			if toleratedBusy(err) {
				continue
			}
			if err != nil {
				errs <- err
				return
			}
			if err := s.ReturnUnknown(ctx, l, b.Ops, 0); err != nil && !toleratedBusy(err) {
				errs <- err
			}
		}
	}()
	wg.Wait()
	if err := <-done; err != nil {
		t.Fatalf("other process: %v\n%s", err, out)
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	childKept := 30
	if m := regexp.MustCompile(`KEPT=(\d+)`).FindSubmatch(out); m != nil {
		childKept, _ = strconv.Atoi(string(m[1]))
	} else if runtime.GOOS == "windows" {
		t.Fatalf("the other process did not report: %s", out)
	}
	if runtime.GOOS != "windows" && childKept != 30 {
		t.Fatalf("writes failed: child kept %d", childKept)
	}
	if n, _ := s.Count(ctx); n != 60+childKept {
		t.Fatalf("count = %d, want %d", n, 60+childKept)
	}
}

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
// drops all of the process's POSIX locks on it, so OpenProfile must never
// open and close the database file itself.
func TestOpenKeepsAnotherStoresLock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), DirName, "profiles", "x", FileName)
	s, err := OpenProfile(path, acct)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	opened := make(chan struct{})
	var probe string
	err = s.tx(ctx, func(*sql.Tx) error { // holds the write lock
		// A second Store opens meanwhile (its schema check then waits for
		// the lock, so let it run aside and finish after the commit).
		go func() {
			defer close(opened)
			if s2, err := OpenProfile(path, acct); err == nil {
				s2.Close()
			}
		}()
		time.Sleep(300 * time.Millisecond)
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperLockProbe$", "-test.count=1")
		cmd.Env = append(os.Environ(), lockProbeEnv+"="+path)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("probe: %v\n%s", err, out)
		}
		probe = string(out)
		return nil
	})
	<-opened
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(probe, "LOCK-BUSY") {
		t.Fatalf("another process took the write lock while this one held it:\n%s", probe)
	}
}

// A writer that outlasts SQLite's own busy wait costs the next save time, not the save.
func TestSaveOutlastsBusyWait(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	s, err := OpenProfile(path, acct)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db, err := sql.Open("sqlite", dsn(path, false, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	st, err := OpenProfile(path, acct)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// A store that gives up at once, like a writer that outlasted the busy wait.
	if _, err := st.db.ExecContext(ctx, "PRAGMA busy_timeout = 0"); err != nil {
		t.Fatal(err)
	}
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := st.Save(ctx, NewNote{ProjectKey: GlobalProject, Title: "waited", Body: "for its turn", Source: SourceCLI})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("save finished while the other writer held the lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := c.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Waiting ends with the caller's context.
func TestBeginStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	path := filepath.Join(t.TempDir(), FileName)
	s, err := OpenProfile(path, acct)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(context.Background(), "PRAGMA busy_timeout = 0"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn(path, false, 0))
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
		t.Fatal(err)
	}
	defer c.ExecContext(context.Background(), "ROLLBACK")
	t0 := time.Now()
	if _, err := s.Save(ctx, NewNote{ProjectKey: GlobalProject, Title: "x", Body: "y", Source: SourceCLI}); err == nil {
		t.Fatal("saved while the other writer held the lock")
	}
	if time.Since(t0) > 5*time.Second {
		t.Fatalf("took %v", time.Since(t0))
	}
}
