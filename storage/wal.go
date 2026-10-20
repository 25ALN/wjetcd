package storage

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// WAL: Write-Ahead Log 预写日志
type WAL struct {
	file *os.File
	mu   sync.Mutex
}

func NewWAL(path string) (*WAL, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		_ = file.Close()
		return nil, err
	}

	return &WAL{
		file: file,
	}, nil
}

// 写入日志条目
func (w *WAL) Write(data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return os.ErrClosed
	}

	_, err := w.file.Write(data)
	if err != nil {
		return err
	}

	// fsync确保数据写到磁盘
	return w.file.Sync()
}

// 写入编码后的条目
func (w *WAL) WriteEntry(entry interface{}) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return w.Write(append(data, '\n'))
}

// 关闭日志文件
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

// 读取所有日志
func (w *WAL) ReadAll() ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return nil, os.ErrClosed
	}
	if _, err := w.file.Seek(0, 0); err != nil {
		return nil, err
	}
	return io.ReadAll(w.file)
}

// 读取所有条目
func (w *WAL) ReadEntries() ([]interface{}, error) {
	data, err := w.ReadAll()
	if err != nil {
		return nil, err
	}
	return decodeEntries(data)
}

// ReadEntriesFrom replays only records written after offset. The offset must
// be at the beginning of the file or immediately after a complete record.
func (w *WAL) ReadEntriesFrom(offset int64) ([]interface{}, error) {
	data, err := w.ReadAll()
	if err != nil {
		return nil, err
	}
	if offset < 0 || offset > int64(len(data)) {
		return nil, fmt.Errorf("WAL offset %d is outside file size %d", offset, len(data))
	}
	if offset > 0 && data[offset-1] != '\n' {
		return nil, fmt.Errorf("WAL offset %d is not on a record boundary", offset)
	}
	return decodeEntries(data[offset:])
}

func decodeEntries(data []byte) ([]interface{}, error) {
	entries := []interface{}{}
	// A crash can interrupt the last append. Only a newline-terminated record
	// is committed to the journal; ignore an incomplete final fragment.
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if lastNewline := strings.LastIndexByte(string(data), '\n'); lastNewline >= 0 {
			data = data[:lastNewline+1]
		} else {
			return entries, nil
		}
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		var entry interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// 清空日志
func (w *WAL) Clear() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return os.ErrClosed
	}

	// Replace the file instead of truncating it in place. The inode acts as a
	// generation marker: a snapshot can tell whether the WAL was cleared after
	// it was written and therefore whether its offset is still meaningful.
	path := w.file.Name()
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	newFile, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		_ = w.file.Close()
		w.file = nil
		return err
	}
	oldFile := w.file
	w.file = newFile
	closeErr := oldFile.Close()
	dirErr := syncDirectory(filepath.Dir(path))
	if closeErr != nil {
		return closeErr
	}
	return dirErr
}

// 截断日志到指定索引
func (w *WAL) Truncate(index int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return os.ErrClosed
	}
	if index <= 0 {
		return nil
	}
	if _, err := w.file.Seek(0, 0); err != nil {
		return err
	}
	data, err := io.ReadAll(w.file)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if index >= len(lines) {
		if err := w.file.Truncate(0); err != nil {
			return err
		}
		return w.file.Sync()
	}
	remaining := strings.Join(lines[index:], "\n") + "\n"
	if err := w.file.Truncate(0); err != nil {
		return err
	}
	if _, err := w.file.Seek(0, 0); err != nil {
		return err
	}
	if _, err := w.file.Write([]byte(remaining)); err != nil {
		return err
	}
	if err := w.file.Truncate(int64(len(remaining))); err != nil {
		return err
	}
	return w.file.Sync()
}

// 获取日志大小
func (w *WAL) Size() (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}

	stat, err := w.file.Stat()
	if err != nil {
		return 0, err
	}
	return stat.Size(), nil
}

// FileID returns the filesystem identity of the currently open WAL. Snapshot
// recovery uses it to distinguish an uncleared journal from a new journal
// created after checkpointing.
func (w *WAL) FileID() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return "", os.ErrClosed
	}
	info, err := w.file.Stat()
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("unsupported WAL file metadata")
	}
	return fmt.Sprintf("%d:%d", uint64(stat.Dev), uint64(stat.Ino)), nil
}

// Snapshot: 快照管理
type Snapshot struct {
	data      []byte
	term      int
	idx       int
	walOffset int64
	walID     string
	file      *os.File
	mu        sync.RWMutex
}

func NewSnapshot(path string) (*Snapshot, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		_ = file.Close()
		return nil, err
	}

	s := &Snapshot{
		data: []byte{},
		term: 0,
		idx:  0,
		file: file,
	}

	// 尝试加载现有快照
	if err := s.loadFromFile(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return s, nil
}

func (s *Snapshot) Save(data []byte, term, idx int, walOffset ...int64) error {
	return s.SaveWithWAL(data, term, idx, "", firstOffset(walOffset))
}

func firstOffset(offsets []int64) int64 {
	if len(offsets) == 0 {
		return 0
	}
	return offsets[0]
}

func (s *Snapshot) SaveWithWAL(data []byte, term, idx int, walID string, walOffset int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return os.ErrClosed
	}
	path := s.file.Name()

	// Write a complete temporary file and atomically replace the old snapshot.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(tmp)
	err = encoder.Encode(map[string]interface{}{
		"data":       data,
		"term":       term,
		"idx":        idx,
		"wal_offset": walOffset,
		"wal_id":     walID,
	})
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	newFile, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	oldFile := s.file
	s.file = newFile
	if err := oldFile.Close(); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	s.data = append([]byte(nil), data...)
	s.term = term
	s.idx = idx
	s.walOffset = walOffset
	s.walID = walID
	return nil
}

func (s *Snapshot) loadFromFile() error {
	info, err := s.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return nil
	}
	if _, err := s.file.Seek(0, 0); err != nil {
		return err
	}
	var m map[string]interface{}
	decoder := json.NewDecoder(s.file)
	if err := decoder.Decode(&m); err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("snapshot must contain a JSON object")
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("snapshot contains trailing JSON value")
		}
		return err
	}

	data, ok := m["data"].(string)
	if !ok {
		return fmt.Errorf("snapshot data field is missing or invalid")
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return fmt.Errorf("decode snapshot data: %w", err)
	}
	s.data = decoded
	if term, ok := m["term"].(float64); ok {
		s.term = int(term)
	} else {
		return fmt.Errorf("snapshot term field is missing or invalid")
	}
	if idx, ok := m["idx"].(float64); ok {
		s.idx = int(idx)
	} else {
		return fmt.Errorf("snapshot index field is missing or invalid")
	}
	if offset, ok := m["wal_offset"].(float64); ok {
		s.walOffset = int64(offset)
	}
	if walID, ok := m["wal_id"].(string); ok {
		s.walID = walID
	}
	return nil
}

func (s *Snapshot) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// 获取快照大小
func (s *Snapshot) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}

// 检查快照是否存在
func (s *Snapshot) Exists() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data) > 0
}

// 删除快照
func (s *Snapshot) Delete() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return os.ErrClosed
	}

	s.data = []byte{}
	s.term = 0
	s.idx = 0
	s.walOffset = 0
	s.walID = ""

	if err := s.file.Truncate(0); err != nil {
		return err
	}
	if _, err := s.file.Seek(0, 0); err != nil {
		return err
	}
	return s.file.Sync()
}

func (s *Snapshot) Load() ([]byte, int, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data := make([]byte, len(s.data))
	copy(data, s.data)
	return data, s.term, s.idx
}

func (s *Snapshot) WALOffset() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.walOffset
}

func (s *Snapshot) WALID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.walID
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
