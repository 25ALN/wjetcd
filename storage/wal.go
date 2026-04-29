package storage

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
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

	return &WAL{
		file: file,
	}, nil
}

// 写入日志条目
func (w *WAL) Write(data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

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
		return w.file.Close()
	}
	return nil
}

// 读取所有日志
func (w *WAL) ReadAll() ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.file.Seek(0, 0)
	data := make([]byte, 0)

	buf := make([]byte, 1024)
	for {
		n, err := w.file.Read(buf)
		if err != nil {
			break
		}
		data = append(data, buf[:n]...)
	}

	return data, nil
}

// 读取所有条目
func (w *WAL) ReadEntries() ([]interface{}, error) {
	data, err := w.ReadAll()
	if err != nil {
		return nil, err
	}

	entries := []interface{}{}
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
	return w.file.Truncate(0)
}

// 截断日志到指定索引
func (w *WAL) Truncate(index int) error {
	return w.Clear()
}

// 获取日志大小
func (w *WAL) Size() (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	stat, err := w.file.Stat()
	if err != nil {
		return 0, err
	}
	return stat.Size(), nil
}

// Snapshot: 快照管理
type Snapshot struct {
	data []byte
	term int
	idx  int
	file *os.File
	mu   sync.RWMutex
}

func NewSnapshot(path string) (*Snapshot, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}

	s := &Snapshot{
		data: []byte{},
		term: 0,
		idx:  0,
		file: file,
	}

	// 尝试加载现有快照
	s.loadFromFile()
	return s, nil
}

func (s *Snapshot) Save(data []byte, term, idx int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data = make([]byte, len(data))
	copy(s.data, data)
	s.term = term
	s.idx = idx

	// 写入文件
	s.file.Seek(0, 0)  //将指针移到文件开头
	s.file.Truncate(0) //清空文件内容
	encoder := json.NewEncoder(s.file)
	err := encoder.Encode(map[string]interface{}{
		"data": s.data,
		"term": s.term,
		"idx":  s.idx,
	})
	if err != nil {
		return err
	}
	return s.file.Sync()
}

func (s *Snapshot) loadFromFile() {
	s.file.Seek(0, 0)
	var m map[string]interface{}
	decoder := json.NewDecoder(s.file)
	if err := decoder.Decode(&m); err != nil {
		return
	}

	if data, ok := m["data"].([]byte); ok {
		s.data = data
	}
	if term, ok := m["term"].(float64); ok {
		s.term = int(term)
	}
	if idx, ok := m["idx"].(float64); ok {
		s.idx = int(idx)
	}
}

func (s *Snapshot) Close() error {
	return s.file.Close()
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

	s.data = []byte{}
	s.term = 0
	s.idx = 0

	return s.file.Truncate(0)
}

func (s *Snapshot) Load() ([]byte, int, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data := make([]byte, len(s.data))
	copy(data, s.data)
	return data, s.term, s.idx
}
