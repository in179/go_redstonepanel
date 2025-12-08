package instancecontrol

import (
	"sync"
	"testing"
	"time"
)

type fakeDB struct {
	mu sync.Mutex
	m  map[string]interface{}
}

func newFakeDB() *fakeDB {
	return &fakeDB{m: make(map[string]interface{})}
}

func (f *fakeDB) Set(key string, value interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[key] = value
	return nil
}

func (f *fakeDB) Get(key string) (interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[key]
	if !ok {
		return nil, ErrKeyNotFound
	}
	return v, nil
}

var ErrKeyNotFound = &keyNotFoundErr{}

type keyNotFoundErr struct{}

func (e *keyNotFoundErr) Error() string { return "key not found" }

func TestSaveLoadRecord(t *testing.T) {
	db := newFakeDB()
	c := NewController(WithDB(db), WithUseAbsPaths(false))
	path := "inst1"
	rec := InstanceRecord{State: StateStarting, PID: 123}
	if err := c.saveRecord(path, rec); err != nil {
		t.Fatalf("saveRecord error: %v", err)
	}
	loaded, err := c.loadRecord(path)
	if err != nil {
		t.Fatalf("loadRecord error: %v", err)
	}
	if loaded.State != StateStarting {
		t.Fatalf("expected state %s got %s", StateStarting, loaded.State)
	}
	if loaded.PID != 123 {
		t.Fatalf("expected pid 123 got %d", loaded.PID)
	}
}

func TestStartStop(t *testing.T) {
	db := newFakeDB()
	c := NewController(WithDB(db), WithUseAbsPaths(false), WithGracePeriod(2*time.Second))
	dir := t.TempDir()
	cmd := []string{"sh", "-c", "sleep 2"}
	if err := c.Start(dir, cmd); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	state, err := c.GetState(dir)
	if err != nil {
		t.Fatalf("GetState error: %v", err)
	}
	if state.State != StateRunning {
		t.Fatalf("expected running state got %s", state.State)
	}
	if state.PID == 0 {
		t.Fatalf("expected non-zero pid")
	}
	if err := c.Stop(dir); err != nil {
		t.Fatalf("Stop error: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	state2, err := c.GetState(dir)
	if err != nil {
		t.Fatalf("GetState after stop error: %v", err)
	}
	if state2.State != StateStopped {
		t.Fatalf("expected stopped state got %s", state2.State)
	}
}
