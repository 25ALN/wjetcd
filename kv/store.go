package kv

import (
	"fmt"
	"math"
	"sync"
)

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
		}
		entry := &VersionEntry{
			Revision:  rev,
			Value:     cmd.Value,
			Tombstone: false,
		}
		kv.data[cmd.Key] = append(kv.data[cmd.Key], entry)
		if cmd.LeaseID > 0 {
			kv.key2Lease[cmd.Key] = cmd.LeaseID
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
			kv.currentRev++
			rev := kv.currentRev
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
			kv.currentRev++
			rev := kv.currentRev
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
			kv.currentRev++
			entry := &VersionEntry{
				Revision:  kv.currentRev,
				Value:     "",
				Tombstone: true,
			}
			kv.data[key] = append(kv.data[key], entry)
			delete(kv.key2Lease, key)
			deletedKeys = append(deletedKeys, key)
		}
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
