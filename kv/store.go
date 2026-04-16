package kv

import (
	"fmt"
	"sync"
)

type KVStore struct {
	mu   sync.RWMutex
	data map[string]string
}

func NewKVStore() *KVStore {
	return &KVStore{
		data: make(map[string]string),
	}
}

// Apply：由 Raft 调用来应用命令
func (kv *KVStore) Apply(cmd Command) (string, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	switch cmd.Type {
	case CmdPut:
		kv.data[cmd.Key] = cmd.Value
		return "", nil

	case CmdGet:
		val, ok := kv.data[cmd.Key]
		if !ok {
			return "", nil
		}
		return val, nil

	case CmdDelete:
		delete(kv.data, cmd.Key)
		return "", nil

	default:
		return "", fmt.Errorf("unknown command type")
	}
}

func (kv *KVStore) Get(key string) string {
	kv.mu.RLock()
	defer kv.mu.RUnlock()

	val, ok := kv.data[key]
	if !ok {
		return ""
	}
	return val
}

// 直接写入
func (kv *KVStore) Put(key, value string) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	kv.data[key] = value
}

// 直接删除
func (kv *KVStore) Delete(key string) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	delete(kv.data, key)
}

// 获取所有键
func (kv *KVStore) Keys() []string {
	kv.mu.RLock()
	defer kv.mu.RUnlock()

	keys := make([]string, 0, len(kv.data))
	for k := range kv.data {
		keys = append(keys, k)
	}
	return keys
}

// 获取存储大小
func (kv *KVStore) Size() int {
	kv.mu.RLock()
	defer kv.mu.RUnlock()

	return len(kv.data)
}

// 检查键是否存在
func (kv *KVStore) Exists(key string) bool {
	kv.mu.RLock()
	defer kv.mu.RUnlock()

	_, ok := kv.data[key]
	return ok
}

// 清空存储
func (kv *KVStore) Clear() {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	kv.data = make(map[string]string)
}
