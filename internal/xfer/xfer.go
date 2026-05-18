// Package xfer is the concurrent transfer engine for taildrives.
package xfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/archn3m3sis/taildrives/internal/webdav"
)

// State is a job's lifecycle state.
type State int

const (
	Queued State = iota
	Running
	Done
	Failed
)

func (s State) String() string {
	return [...]string{"queued", "running", "done", "failed"}[s]
}

// Job is one source→destination transfer.
type Job struct {
	ID    int64
	Src   string // for uploads from local: "file:///abs/path"
	Dst   string // WebDAV path or "file:///abs/path" for downloads
	Bytes int64

	mu      sync.Mutex
	state   State
	written int64
	err     error
	started time.Time
	done    time.Time
}

// Snapshot is a point-in-time view safe for the UI.
type Snapshot struct {
	ID      int64
	Src     string
	Dst     string
	Bytes   int64
	Written int64
	State   State
	Err     string
	Elapsed time.Duration
}

func (j *Job) snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := Snapshot{
		ID: j.ID, Src: j.Src, Dst: j.Dst, Bytes: j.Bytes,
		Written: j.written, State: j.state,
	}
	if j.err != nil {
		s.Err = j.err.Error()
	}
	if !j.started.IsZero() {
		end := j.done
		if end.IsZero() {
			end = time.Now()
		}
		s.Elapsed = end.Sub(j.started)
	}
	return s
}

// Manager runs jobs concurrently.
type Manager struct {
	Client      *webdav.Client
	Concurrency int

	jobs    sync.Map      // ID → *Job
	queue   chan *Job
	wg      sync.WaitGroup
	once    sync.Once
	idGen   atomic.Int64
	started atomic.Bool
	stopped atomic.Bool
}

// New returns a manager. Concurrency defaults to 4.
func New(c *webdav.Client, concurrency int) *Manager {
	if concurrency <= 0 {
		concurrency = 4
	}
	return &Manager{
		Client:      c,
		Concurrency: concurrency,
		queue:       make(chan *Job, 256),
	}
}

// Start spins up workers.
func (m *Manager) Start(ctx context.Context) {
	m.once.Do(func() {
		m.started.Store(true)
		for i := 0; i < m.Concurrency; i++ {
			m.wg.Add(1)
			go m.worker(ctx)
		}
	})
}

// Stop drains and waits.
func (m *Manager) Stop() {
	if !m.started.Load() || m.stopped.Load() {
		return
	}
	m.stopped.Store(true)
	close(m.queue)
	m.wg.Wait()
}

// Submit a single transfer.
func (m *Manager) Submit(src, dst string) *Job {
	id := m.idGen.Add(1)
	j := &Job{ID: id, Src: src, Dst: dst}
	m.jobs.Store(id, j)
	m.queue <- j
	return j
}

// SubmitBulk creates one job per destination, all reading the same source.
func (m *Manager) SubmitBulk(src string, dsts []string) []*Job {
	out := make([]*Job, 0, len(dsts))
	for _, d := range dsts {
		out = append(out, m.Submit(src, d))
	}
	return out
}

// Snapshots returns the current state of every known job, newest-first.
func (m *Manager) Snapshots() []Snapshot {
	var snaps []Snapshot
	m.jobs.Range(func(_, v any) bool {
		j := v.(*Job)
		snaps = append(snaps, j.snapshot())
		return true
	})
	// sort newest first by ID
	for i := 0; i < len(snaps); i++ {
		for j := i + 1; j < len(snaps); j++ {
			if snaps[j].ID > snaps[i].ID {
				snaps[i], snaps[j] = snaps[j], snaps[i]
			}
		}
	}
	return snaps
}

// Get returns a snapshot by ID.
func (m *Manager) Get(id int64) (Snapshot, bool) {
	v, ok := m.jobs.Load(id)
	if !ok {
		return Snapshot{}, false
	}
	return v.(*Job).snapshot(), true
}

func (m *Manager) worker(ctx context.Context) {
	defer m.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case j, ok := <-m.queue:
			if !ok {
				return
			}
			m.run(ctx, j)
		}
	}
}

func (m *Manager) setState(j *Job, s State, err error) {
	j.mu.Lock()
	j.state = s
	if err != nil {
		j.err = err
	}
	if s == Running && j.started.IsZero() {
		j.started = time.Now()
	}
	if s == Done || s == Failed {
		j.done = time.Now()
	}
	j.mu.Unlock()
}

const filePrefix = "file://"

func isLocal(p string) bool { return strings.HasPrefix(p, filePrefix) }
func localPath(p string) string {
	return strings.TrimPrefix(p, filePrefix)
}

// run dispatches based on src/dst kind:
//   - local → remote   : PUT (upload)
//   - remote → local   : GET (download)
//   - remote → remote  : COPY (same-share fast path) or stream
//   - local → local    : os.Copy (rare; here for symmetry)
func (m *Manager) run(ctx context.Context, j *Job) {
	m.setState(j, Running, nil)
	var err error
	switch {
	case isLocal(j.Src) && !isLocal(j.Dst):
		err = m.upload(ctx, j)
	case !isLocal(j.Src) && isLocal(j.Dst):
		err = m.download(ctx, j)
	case !isLocal(j.Src) && !isLocal(j.Dst):
		err = m.copyRemote(ctx, j)
	default:
		err = m.localToLocal(j)
	}
	if err != nil {
		m.setState(j, Failed, err)
	} else {
		m.setState(j, Done, nil)
	}
}

// progressReader wraps a reader and updates j.written as it goes.
type progressReader struct {
	r io.Reader
	j *Job
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	if n > 0 {
		p.j.mu.Lock()
		p.j.written += int64(n)
		p.j.mu.Unlock()
	}
	return n, err
}

func (m *Manager) upload(_ context.Context, j *Job) error {
	src := localPath(j.Src)
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return m.uploadDir(src, j.Dst, j)
	}
	j.mu.Lock()
	j.Bytes = info.Size()
	j.mu.Unlock()
	// ensure parent collection exists
	if err := m.ensureParent(j.Dst); err != nil {
		return err
	}
	return m.Client.Put(j.Dst, &progressReader{r: f, j: j}, info.Size())
}

func (m *Manager) uploadDir(srcDir, dstDir string, j *Job) error {
	var totalBytes int64
	// pre-walk to count bytes for progress
	_ = walkLocal(srcDir, func(p string, info os.FileInfo) error {
		if !info.IsDir() {
			totalBytes += info.Size()
		}
		return nil
	})
	j.mu.Lock()
	j.Bytes = totalBytes
	j.mu.Unlock()

	if err := m.Client.Mkdir(dstDir); err != nil {
		return err
	}
	return walkLocal(srcDir, func(p string, info os.FileInfo) error {
		rel, err := relpath(srcDir, p)
		if err != nil {
			return err
		}
		dst := path.Join(dstDir, rel)
		if info.IsDir() {
			return m.Client.Mkdir(dst)
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		return m.Client.Put(dst, &progressReader{r: f, j: j}, info.Size())
	})
}

func (m *Manager) download(_ context.Context, j *Job) error {
	// Recursive download if source is a directory.
	if m.isRemoteDir(j.Src) {
		return m.downloadDir(j)
	}
	data, err := m.Client.Get(j.Src)
	if err != nil {
		return err
	}
	j.mu.Lock()
	j.Bytes = int64(len(data))
	j.mu.Unlock()
	dst := localPath(j.Dst)
	if err := os.MkdirAll(path.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return err
	}
	j.mu.Lock()
	j.written = int64(len(data))
	j.mu.Unlock()
	return nil
}

func (m *Manager) copyRemote(_ context.Context, j *Job) error {
	// Detect directory vs file via PROPFIND. Directories list; files 404.
	srcIsDir := m.isRemoteDir(j.Src)

	// Try server-side COPY first (instant for same-share, even recursive).
	if err := m.Client.Copy(j.Src, j.Dst, true); err == nil {
		// Optionally fill bytes from a PROPFIND on dst; for now best-effort.
		j.mu.Lock()
		if j.Bytes == 0 {
			j.Bytes = 1
		}
		j.written = j.Bytes
		j.mu.Unlock()
		return nil
	} else if !errors.Is(err, webdav.ErrUnsupported) {
		// Cross-share copies typically fail here with 502/501; fall through.
	}

	if srcIsDir {
		return m.copyRemoteDir(j)
	}
	return m.copyRemoteFile(j, j.Src, j.Dst)
}

// isRemoteDir returns true if PROPFIND succeeds on p (i.e. p is a collection).
func (m *Manager) isRemoteDir(p string) bool {
	_, err := m.Client.List(p)
	return err == nil
}

func (m *Manager) copyRemoteFile(j *Job, src, dst string) error {
	data, err := m.Client.Get(src)
	if err != nil {
		return fmt.Errorf("GET %s: %w", src, err)
	}
	if err := m.ensureParent(dst); err != nil {
		return err
	}
	if err := m.Client.Put(dst, bytes.NewReader(data), int64(len(data))); err != nil {
		return fmt.Errorf("PUT %s: %w", dst, err)
	}
	j.mu.Lock()
	j.written += int64(len(data))
	if j.Bytes < j.written {
		j.Bytes = j.written
	}
	j.mu.Unlock()
	return nil
}

// copyRemoteDir walks the source tree and copies each file, creating
// destination collections as it goes. Use this for cross-share directory
// copies where server-side COPY isn't supported.
func (m *Manager) copyRemoteDir(j *Job) error {
	// Pre-walk for total byte count so progress reflects reality.
	total, err := m.walkRemoteSize(j.Src)
	if err != nil {
		return err
	}
	j.mu.Lock()
	j.Bytes = total
	j.mu.Unlock()
	return m.walkAndCopy(j, j.Src, j.Dst)
}

func (m *Manager) walkAndCopy(j *Job, srcDir, dstDir string) error {
	if err := m.Client.Mkdir(dstDir); err != nil {
		return err
	}
	entries, err := m.Client.List(srcDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcChild := strings.TrimRight(srcDir, "/") + "/" + e.Name
		dstChild := strings.TrimRight(dstDir, "/") + "/" + e.Name
		if e.IsDir {
			if err := m.walkAndCopy(j, srcChild, dstChild); err != nil {
				return err
			}
			continue
		}
		if err := m.copyRemoteFile(j, srcChild, dstChild); err != nil {
			return err
		}
	}
	return nil
}

// walkRemoteSize sums the byte size of every file under srcDir.
func (m *Manager) walkRemoteSize(srcDir string) (int64, error) {
	var total int64
	entries, err := m.Client.List(srcDir)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if e.IsDir {
			sub, err := m.walkRemoteSize(strings.TrimRight(srcDir, "/") + "/" + e.Name)
			if err != nil {
				return total, err
			}
			total += sub
		} else {
			total += e.Size
		}
	}
	return total, nil
}

// downloadDir walks a remote directory tree and writes each file to the local
// filesystem under j.Dst (which is file:// or a plain path).
func (m *Manager) downloadDir(j *Job) error {
	total, err := m.walkRemoteSize(j.Src)
	if err != nil {
		return err
	}
	j.mu.Lock()
	j.Bytes = total
	j.mu.Unlock()
	dstRoot := localPath(j.Dst)
	return m.walkAndDownload(j, j.Src, dstRoot)
}

func (m *Manager) walkAndDownload(j *Job, srcDir, dstDir string) error {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	entries, err := m.Client.List(srcDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcChild := strings.TrimRight(srcDir, "/") + "/" + e.Name
		dstChild := path.Join(dstDir, e.Name)
		if e.IsDir {
			if err := m.walkAndDownload(j, srcChild, dstChild); err != nil {
				return err
			}
			continue
		}
		data, err := m.Client.Get(srcChild)
		if err != nil {
			return fmt.Errorf("GET %s: %w", srcChild, err)
		}
		if err := os.WriteFile(dstChild, data, 0o644); err != nil {
			return err
		}
		j.mu.Lock()
		j.written += int64(len(data))
		j.mu.Unlock()
	}
	return nil
}

// localToLocal copies a file or directory tree between two local filesystem
// paths. Used when both src and dst are file:// — e.g. downloading from the
// local-host's own Taildrive share (which resolves to a filesystem path).
func (m *Manager) localToLocal(j *Job) error {
	srcPath := localPath(j.Src)
	dstPath := localPath(j.Dst)
	info, err := os.Stat(srcPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return m.copyLocalDir(srcPath, dstPath, j)
	}
	j.mu.Lock()
	j.Bytes = info.Size()
	j.mu.Unlock()
	return copyLocalFile(srcPath, dstPath, j)
}

func copyLocalFile(srcPath, dstPath string, j *Job) error {
	if err := os.MkdirAll(path.Dir(dstPath), 0o755); err != nil {
		return err
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	written, err := io.Copy(out, &progressReader{r: in, j: j})
	if err != nil {
		return err
	}
	_ = written
	return nil
}

func (m *Manager) copyLocalDir(srcDir, dstDir string, j *Job) error {
	var totalBytes int64
	_ = walkLocal(srcDir, func(_ string, info os.FileInfo) error {
		if !info.IsDir() {
			totalBytes += info.Size()
		}
		return nil
	})
	j.mu.Lock()
	j.Bytes = totalBytes
	j.mu.Unlock()
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	return walkLocal(srcDir, func(p string, info os.FileInfo) error {
		rel, err := relpath(srcDir, p)
		if err != nil {
			return err
		}
		dst := path.Join(dstDir, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		return copyLocalFile(p, dst, j)
	})
}

// ensureParent creates the parent directory of dst (best-effort, idempotent).
func (m *Manager) ensureParent(dst string) error {
	parent := path.Dir(dst)
	if parent == "" || parent == "/" || parent == "." {
		return nil
	}
	// MKCOL is best-effort — already-exists returns 405 which we treat as ok.
	return m.Client.Mkdir(parent)
}

// walkLocal is a tiny io/fs-free walker (we want explicit symlink behavior).
func walkLocal(root string, fn func(p string, info os.FileInfo) error) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if err := fn(root, info); err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		full := path.Join(root, e.Name())
		if err := walkLocal(full, fn); err != nil {
			return err
		}
	}
	return nil
}

func relpath(base, p string) (string, error) {
	base = strings.TrimRight(base, "/")
	p = strings.TrimRight(p, "/")
	if !strings.HasPrefix(p, base) {
		return "", fmt.Errorf("path %q not under base %q", p, base)
	}
	rel := strings.TrimPrefix(p, base)
	rel = strings.TrimLeft(rel, "/")
	return rel, nil
}
