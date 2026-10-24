package memory

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Second security review regression: concurrent first opens of a new
// store (device notebook or account profile, read-write or read-only)
// all succeed and leave a usable store; previously a racing opener reran
// the v1 DDL after another finished v2 and bricked the file.
func TestConcurrentFirstOpen(t *testing.T) {
	for _, p := range []Profile{Local, acct} {
		for i := 0; i < 20; i++ {
			path := filepath.Join(t.TempDir(), "m.db")
			var wg sync.WaitGroup
			errs := make([]error, 6)
			for j := 0; j < 6; j++ {
				wg.Add(1)
				go func(j int) {
					defer wg.Done()
					var s *Store
					var err error
					if j%3 == 2 {
						s, err = OpenProfileReadOnly(path, p, 5*time.Second)
						if err == ErrNoStore {
							err = nil
						}
					} else {
						s, err = OpenProfile(path, p)
					}
					errs[j] = err
					if s != nil {
						s.Close()
					}
				}(j)
			}
			wg.Wait()
			for _, e := range errs {
				if e != nil {
					t.Fatalf("%+v iteration %d: open failed: %v", p, i, e)
				}
			}
			s, err := OpenProfile(path, p)
			if err != nil {
				t.Fatalf("%+v iteration %d: store left unusable: %v", p, i, err)
			}
			s.Close()
		}
	}
}
