package kv

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"
)

// storeSnapshot is the on-disk representation of the MVCC state. It is kept
// private so callers cannot mutate the live store through a snapshot value.
type storeSnapshot struct {
	Data       map[string][]VersionEntry `json:"data"`
	Key2Lease  map[string]int64          `json:"key2_lease"`
	CurrentRev int64                     `json:"current_revision"`
}

type VersionEntry struct {
	Revision  int64
	Value     string
	Tombstone bool
}

type KVStore struct {
	mu          sync.RWMutex
	data        map[string][]*VersionEntry
	key2Lease   map[string]int64
	currentRev  int64
	watchEvents []WatchEvent
	watchMu     sync.RWMutex
}

type WatchEvent struct {
	Key      string
	Value    string
	Type     string
	Revision int64
}

func NewKVStore() *KVStore {
	return &KVStore{
		data:      make(map[string][]*VersionEntry),
		key2Lease: make(map[string]int64),
	}
}

func (kv *KVStore) SetKeyLease(key string, leaseId int64) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.key2Lease[key] = leaseId
}

func (kv *KVStore) GetKeyLease(key string) int64 {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	return kv.key2Lease[key]
}

func (kv *KVStore) GetCurrentRevision() int64 {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	return kv.currentRev
}

func (kv *KVStore) IncrementRevision() int64 {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.currentRev++
	return kv.currentRev
}

// Apply：由 Raft 调用来应用命令
func (kv *KVStore) Apply(cmd Command) (string, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	switch cmd.Type {
	case CmdPut:
		rev := cmd.Revision
		if rev == 0 {
			kv.currentRev++
			rev = kv.currentRev
		} else if rev > kv.currentRev {
			kv.currentRev = rev
		}
		entry := &VersionEntry{
			Revision:  rev,
			Value:     cmd.Value,
			Tombstone: false,
		}
		kv.data[cmd.Key] = append(kv.data[cmd.Key], entry)
		if cmd.LeaseID > 0 {
			kv.key2Lease[cmd.Key] = cmd.LeaseID
		} else {
			delete(kv.key2Lease, cmd.Key)
		}
		return "", nil

	case CmdGet:
		val, _ := kv.GetAtRevision(cmd.Key, cmd.Revision)
		return val, nil

	case CmdDelete:
		rev := cmd.Revision
		if rev == 0 {
			kv.currentRev++
			rev = kv.currentRev
		} else if rev > kv.currentRev {
			kv.currentRev = rev
		}
		entry := &VersionEntry{
			Revision:  rev,
			Value:     "",
			Tombstone: true,
		}
		kv.data[cmd.Key] = append(kv.data[cmd.Key], entry)
		delete(kv.key2Lease, cmd.Key)
		return "", nil

	case CmdCAS:
		entries := kv.data[cmd.Key]
		var latest *VersionEntry
		if len(entries) > 0 {
			latest = entries[len(entries)-1]
		}
		if latest != nil && !latest.Tombstone && latest.Revision == cmd.ExpectedRev {
			rev := cmd.Revision
			if rev == 0 {
				kv.currentRev++
				rev = kv.currentRev
			} else if rev > kv.currentRev {
				kv.currentRev = rev
			}
			entry := &VersionEntry{
				Revision:  rev,
				Value:     cmd.Value,
				Tombstone: false,
			}
			kv.data[cmd.Key] = append(kv.data[cmd.Key], entry)
			return "OK", nil
		}
		return "CAS_FAILED", nil
		/**/
	case CmdLeaseAttach:
		if cmd.LeaseID > 0 {
			kv.key2Lease[cmd.Key] = cmd.LeaseID
			rev := cmd.Revision
			if rev == 0 {
				kv.currentRev++
				rev = kv.currentRev
			} else if rev > kv.currentRev {
				kv.currentRev = rev
			}
			entry := &VersionEntry{
				Revision:  rev,
				Value:     cmd.Value,
				Tombstone: false,
			}
			kv.data[cmd.Key] = append(kv.data[cmd.Key], entry)
		}
		return "", nil

	default:
		return "", fmt.Errorf("unknown command type")
	}
}

func (kv *KVStore) GetAtRevision(key string, rev int64) (string, int64) {
	kv.mu.RLock()
	defer kv.mu.RUnlock()

	entries, ok := kv.data[key]
	if !ok || len(entries) == 0 {
		return "", 0
	}

	if rev == 0 {
		rev = math.MaxInt64
	}

	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Revision <= rev {
			if entries[i].Tombstone {
				return "", entries[i].Revision
			}
			return entries[i].Value, entries[i].Revision
		}
	}
	return "", 0
}

func (kv *KVStore) Get(key string) string {
	val, _ := kv.GetAtRevision(key, 0)
	return val
}

func (kv *KVStore) Put(key, value string) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.currentRev++
	entry := &VersionEntry{
		Revision:  kv.currentRev,
		Value:     value,
		Tombstone: false,
	}
	kv.data[key] = append(kv.data[key], entry)
}

func (kv *KVStore) Delete(key string) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.currentRev++
	entry := &VersionEntry{
		Revision:  kv.currentRev,
		Value:     "",
		Tombstone: true,
	}
	kv.data[key] = append(kv.data[key], entry)
}

func (kv *KVStore) Keys() []string {
	kv.mu.RLock()
	defer kv.mu.RUnlock()

	keys := make([]string, 0)
	for k, entries := range kv.data {
		if len(entries) > 0 {
			latest := entries[len(entries)-1]
			if !latest.Tombstone {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

func (kv *KVStore) Size() int {
	kv.mu.RLock()
	defer kv.mu.RUnlock()

	count := 0
	for _, entries := range kv.data {
		if len(entries) > 0 {
			latest := entries[len(entries)-1]
			if !latest.Tombstone {
				count++
			}
		}
	}
	return count
}

func (kv *KVStore) Exists(key string) bool {
	val := kv.Get(key)
	return val != ""
}

func (kv *KVStore) Clear() {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	kv.data = make(map[string][]*VersionEntry)
	kv.currentRev = 0
}

// 获取key的所有版本历史
func (kv *KVStore) GetHistory(key string) []*VersionEntry {
	kv.mu.RLock()
	defer kv.mu.RUnlock()

	entries, ok := kv.data[key]
	if !ok {
		return nil
	}
	result := make([]*VersionEntry, len(entries))
	copy(result, entries)
	return result
}

// GetPrefix 获取指定前缀的所有键值对
func (kv *KVStore) GetPrefix(prefix string) map[string]string {
	kv.mu.RLock()
	defer kv.mu.RUnlock()

	result := make(map[string]string)
	for key, entries := range kv.data {
		if len(prefix) == 0 || len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			if len(entries) > 0 {
				latest := entries[len(entries)-1]
				if !latest.Tombstone {
					result[key] = latest.Value
				}
			}
		}
	}
	return result
}

// DeletePrefix 删除指定前缀的所有键（返回被删除的 key 列表）
func (kv *KVStore) DeletePrefix(prefix string) []string {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	var deletedKeys []string
	for key := range kv.data {
		if len(prefix) == 0 || len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			deletedKeys = append(deletedKeys, key)
		}
	}
	sort.Strings(deletedKeys)
	for _, key := range deletedKeys {
		kv.currentRev++
		entry := &VersionEntry{
			Revision:  kv.currentRev,
			Value:     "",
			Tombstone: true,
		}
		kv.data[key] = append(kv.data[key], entry)
		delete(kv.key2Lease, key)
	}
	return deletedKeys
}

// KeysWithPrefix 获取所有匹配前缀的 key（不包含已删除的）
func (kv *KVStore) KeysWithPrefix(prefix string) []string {
	kv.mu.RLock()
	defer kv.mu.RUnlock()

	keys := make([]string, 0)
	for key, entries := range kv.data {
		if len(prefix) == 0 || len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			if len(entries) > 0 {
				latest := entries[len(entries)-1]
				if !latest.Tombstone {
					keys = append(keys, key)
				}
			}
		}
	}
	return keys
}

// Snapshot returns a complete, self-contained copy of the MVCC state.
func (kv *KVStore) Snapshot() ([]byte, error) {
	kv.mu.RLock()
	defer kv.mu.RUnlock()

	s := storeSnapshot{
		Data:       make(map[string][]VersionEntry, len(kv.data)),
		Key2Lease:  make(map[string]int64, len(kv.key2Lease)),
		CurrentRev: kv.currentRev,
	}
	for key, entries := range kv.data {
		versions := make([]VersionEntry, len(entries))
		for i, entry := range entries {
			if entry != nil {
				versions[i] = *entry
			}
		}
		s.Data[key] = versions
	}
	for key, leaseID := range kv.key2Lease {
		s.Key2Lease[key] = leaseID
	}
	return json.Marshal(s)
}

// Restore replaces the MVCC state from a Snapshot produced by Snapshot.
func (kv *KVStore) Restore(data []byte) error {
	var s storeSnapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s.Data == nil {
		return fmt.Errorf("snapshot data field is missing or invalid")
	}
	if s.Key2Lease == nil {
		return fmt.Errorf("snapshot key-to-lease field is missing or invalid")
	}
	for key, entries := range s.Data {
		for _, entry := range entries {
			if entry.Revision <= 0 || entry.Revision > s.CurrentRev {
				return fmt.Errorf("invalid revision %d in snapshot key %q", entry.Revision, key)
			}
		}
	}

	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.data = make(map[string][]*VersionEntry, len(s.Data))
	for key, entries := range s.Data {
		versions := make([]*VersionEntry, len(entries))
		for i := range entries {
			entry := entries[i]
			versions[i] = &entry
		}
		kv.data[key] = versions
	}
	kv.key2Lease = s.Key2Lease
	kv.currentRev = s.CurrentRev
	return nil
}
