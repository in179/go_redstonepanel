package instancecontrol

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"instance/mine_db"
)

type State string

const (
	StateUnknown  State = "unknown"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateStopped  State = "stopped"
	StateFailed   State = "failed"
)

type InstanceRecord struct {
	State     State     `json:"state"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	StoppedAt time.Time `json:"stopped_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	LastError string    `json:"last_error,omitempty"`
}

type DB interface {
	Set(key string, value interface{}) error
	Get(key string) (interface{}, error)
}

type mineDBAdapter struct{}

func (m mineDBAdapter) Set(key string, value interface{}) error {
	return mine_db.Set(key, value)
}

func (m mineDBAdapter) Get(key string) (interface{}, error) {
	return mine_db.Get(key)
}

type Controller struct {
	gracePeriod time.Duration
	defaultCmd  []string
	useAbsPaths bool
	db          DB
	watchLock   sync.Mutex
}

type Option func(*Controller)

func WithGracePeriod(d time.Duration) Option {
	return func(c *Controller) {
		c.gracePeriod = d
	}
}

func WithDefaultCmd(cmd []string) Option {
	return func(c *Controller) {
		c.defaultCmd = append([]string(nil), cmd...)
	}
}

func WithUseAbsPaths(use bool) Option {
	return func(c *Controller) {
		c.useAbsPaths = use
	}
}

func WithDB(db DB) Option {
	return func(c *Controller) {
		c.db = db
	}
}

func NewController(opts ...Option) *Controller {
	c := &Controller{
		gracePeriod: 10 * time.Second,
		useAbsPaths: true,
		db:          mineDBAdapter{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *Controller) formatDBKey(path string) (string, error) {
	if c.useAbsPaths {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		return "instancecontrol:" + abs, nil
	}
	return "instancecontrol:" + path, nil
}

func (c *Controller) saveRecord(path string, r InstanceRecord) error {
	key, err := c.formatDBKey(path)
	if err != nil {
		return err
	}
	r.UpdatedAt = time.Now().UTC()
	return c.db.Set(key, r)
}

func (c *Controller) loadRecord(path string) (InstanceRecord, error) {
	var r InstanceRecord
	key, err := c.formatDBKey(path)
	if err != nil {
		return r, err
	}
	v, err := c.db.Get(key)
	if err != nil {
		r.State = StateUnknown
		r.UpdatedAt = time.Now().UTC()
		return r, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, err
	}
	return r, nil
}

func isPidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}

func (c *Controller) Start(targetPath string, launchCmd []string) error {
	absPath, err := filepath.Abs(targetPath)
	if err != nil {
		return err
	}
	rec, err := c.loadRecord(absPath)
	if err != nil {
		return err
	}
	if rec.PID != 0 && isPidAlive(rec.PID) {
		return nil
	}
	if len(launchCmd) == 0 && len(c.defaultCmd) == 0 {
		return errors.New("launch command required")
	}
	if len(launchCmd) == 0 {
		launchCmd = append([]string(nil), c.defaultCmd...)
	}
	cmd := exec.Command(launchCmd[0], launchCmd[1:]...)
	cmd.Dir = absPath
	if err := os.MkdirAll(filepath.Join(absPath, "logs"), 0755); err != nil {
		return err
	}
	stdoutF, _ := os.OpenFile(filepath.Join(absPath, "logs", "stdout.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	stderrF, _ := os.OpenFile(filepath.Join(absPath, "logs", "stderr.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if stdoutF != nil {
		cmd.Stdout = stdoutF
	}
	if stderrF != nil {
		cmd.Stderr = stderrF
	}
	if err := c.saveRecord(absPath, InstanceRecord{State: StateStarting}); err != nil {
		if stdoutF != nil {
			_ = stdoutF.Close()
		}
		if stderrF != nil {
			_ = stderrF.Close()
		}
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = c.saveRecord(absPath, InstanceRecord{State: StateFailed, LastError: err.Error()})
		if stdoutF != nil {
			_ = stdoutF.Close()
		}
		if stderrF != nil {
			_ = stderrF.Close()
		}
		return err
	}
	pid := cmd.Process.Pid
	_ = c.saveRecord(absPath, InstanceRecord{State: StateRunning, PID: pid, StartedAt: time.Now().UTC()})
	go c.watchProcess(absPath, cmd, stdoutF, stderrF)
	return nil
}

func (c *Controller) watchProcess(absPath string, cmd *exec.Cmd, stdoutF, stderrF *os.File) {
	c.watchLock.Lock()
	defer c.watchLock.Unlock()
	err := cmd.Wait()
	rec := InstanceRecord{UpdatedAt: time.Now().UTC()}
	if err != nil {
		rec.State = StateFailed
		rec.LastError = err.Error()
	} else {
		rec.State = StateStopped
		rec.StoppedAt = time.Now().UTC()
	}
	rec.PID = 0
	_ = c.saveRecord(absPath, rec)
	if stdoutF != nil {
		_ = stdoutF.Close()
	}
	if stderrF != nil {
		_ = stderrF.Close()
	}
}

func (c *Controller) Stop(targetPath string) error {
	absPath, err := filepath.Abs(targetPath)
	if err != nil {
		return err
	}
	rec, err := c.loadRecord(absPath)
	if err != nil {
		return err
	}
	if rec.PID == 0 || !isPidAlive(rec.PID) {
		_ = c.saveRecord(absPath, InstanceRecord{State: StateStopped, StoppedAt: time.Now().UTC()})
		return nil
	}
	pid := rec.PID
	proc, err := os.FindProcess(pid)
	if err != nil {
		_ = c.saveRecord(absPath, InstanceRecord{State: StateFailed, LastError: err.Error()})
		return err
	}
	_ = proc.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(c.gracePeriod)
	for time.Now().Before(deadline) {
		if !isPidAlive(pid) {
			_ = c.saveRecord(absPath, InstanceRecord{State: StateStopped, StoppedAt: time.Now().UTC()})
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = proc.Kill()
	_ = c.saveRecord(absPath, InstanceRecord{State: StateStopped, StoppedAt: time.Now().UTC()})
	return nil
}

func (c *Controller) GetState(targetPath string) (InstanceRecord, error) {
	absPath, err := filepath.Abs(targetPath)
	var r InstanceRecord
	if err != nil {
		return r, err
	}
	rec, err := c.loadRecord(absPath)
	if err != nil {
		return rec, err
	}
	if rec.PID != 0 && !isPidAlive(rec.PID) {
		rec.State = StateStopped
		rec.PID = 0
		rec.UpdatedAt = time.Now().UTC()
		_ = c.saveRecord(absPath, rec)
	}
	return rec, nil
}
