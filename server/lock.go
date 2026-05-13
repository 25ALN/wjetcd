package server

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"testetcd/kv"
)

var (
	ErrLockNotHeld     = errors.New("lock not held")
	ErrLockAlreadyHeld = errors.New("lock already held by another client")
	ErrLockExpired     = errors.New("lock expired")
)

type LockManager struct {
	mu      sync.Mutex
	locks   map[string]*LockInfo
	leaseMgr *LeaseManager
	store   *kv.KVStore
}

type LockInfo struct {
	Key       string
	LeaseID   int64
	OwnerID   string
	AcquiredAt time.Time
	watcherCh chan struct{}
	cancelFn  func()
}

type LockHandle struct {
	Key     string
	LeaseID int64
	OwnerID string
}

func NewLockManager(leaseMgr *LeaseManager, store *kv.KVStore) *LockManager {
	return &LockManager{
		locks:    make(map[string]*LockInfo),
		leaseMgr: leaseMgr,
		store:    store,
	}
}

func (lm *LockManager) Acquire(key string, ttl int64, ownerID string) (*LockHandle, error) {
	lm.mu.Lock()

	if info, ok := lm.locks[key]; ok {
		if info.LeaseID > 0 && lm.leaseMgr.GetLease(info.LeaseID) != nil {
			lm.mu.Unlock()
			return nil, ErrLockAlreadyHeld
		}
	}

	if ttl <= 0 {
		ttl = 10
	}

	leaseID := lm.leaseMgr.Grant(ttl)

	lm.locks[key] = &LockInfo{
		Key:        key,
		LeaseID:    leaseID,
		OwnerID:    ownerID,
		AcquiredAt: time.Now(),
	}
	lm.mu.Unlock()

	return &LockHandle{
		Key:     key,
		LeaseID: leaseID,
		OwnerID: ownerID,
	}, nil
}

func (lm *LockManager) Release(key string, leaseID int64, ownerID string) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	info, ok := lm.locks[key]
	if !ok {
		return ErrLockNotHeld
	}

	if info.LeaseID != leaseID {
		return ErrLockNotHeld
	}

	if info.OwnerID != ownerID {
		return ErrLockNotHeld
	}

	keys := lm.leaseMgr.RevokeLease(leaseID)
	if len(keys) == 0 && lm.leaseMgr.GetLease(leaseID) == nil {
		return ErrLockExpired
	}

	delete(lm.locks, key)
	return nil
}

func (lm *LockManager) KeepAlive(key string, leaseID int64, ownerID string, ttl int64) error {
	lm.mu.Lock()
	info, ok := lm.locks[key]
	if !ok {
		lm.mu.Unlock()
		return ErrLockNotHeld
	}
	if info.LeaseID != leaseID || info.OwnerID != ownerID {
		lm.mu.Unlock()
		return ErrLockNotHeld
	}
	lm.mu.Unlock()

	if ttl <= 0 {
		ttl = 10
	}
	if !lm.leaseMgr.KeepAlive(leaseID, ttl) {
		return ErrLockExpired
	}
	return nil
}

func (lm *LockManager) GetLockStatus(key string) (bool, int64) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	info, ok := lm.locks[key]
	if !ok {
		return false, 0
	}

	if lm.leaseMgr.GetLease(info.LeaseID) == nil {
		delete(lm.locks, key)
		return false, 0
	}

	return true, info.LeaseID
}

func (lm *LockManager) ListLocks() []string {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	keys := make([]string, 0, len(lm.locks))
	for key := range lm.locks {
		keys = append(keys, key)
	}
	return keys
}

func (lm *LockManager) OnLeaseExpired(leaseID int64, keys []string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	for _, key := range keys {
		if info, ok := lm.locks[key]; ok && info.LeaseID == leaseID {
			delete(lm.locks, key)
		}
	}
}

type LockWithWatch struct {
	Key       string
	LeaseID   int64
	OwnerID   string
	WatcherID uint64
	WatcherCh chan struct{}
}

type TryLockResult int

const (
	TryLockSuccess TryLockResult = iota
	TryLockContended
	TryLockError
)

func (lm *LockManager) TryAcquire(key string, ttl int64, ownerID string, wait bool) (*LockWithWatch, TryLockResult) {
	handle, err := lm.Acquire(key, ttl, ownerID)
	if err != nil {
		if errors.Is(err, ErrLockAlreadyHeld) {
			if wait {
				lm.mu.Lock()
				info, _ := lm.locks[key]
				watcherCh := make(chan struct{}, 1)
				if info != nil {
					info.watcherCh = watcherCh
				}
				lm.mu.Unlock()
				return &LockWithWatch{
					Key:       key,
					OwnerID:   ownerID,
					WatcherCh: watcherCh,
				}, TryLockContended
			}
			return nil, TryLockContended
		}
		return nil, TryLockError
	}

	return &LockWithWatch{
		Key:     key,
		LeaseID: handle.LeaseID,
		OwnerID: ownerID,
	}, TryLockSuccess
}

func (lm *LockManager) GetLockInfo(key string) *LockInfo {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.locks[key]
}

func FormatLockResponse(handle *LockHandle, err error) string {
	if err != nil {
		return fmt.Sprintf(`{"ok":false,"error":"%s"}`, err.Error())
	}
	return fmt.Sprintf(`{"ok":true,"key":"%s","lease_id":%d,"owner_id":"%s"}`,
		handle.Key, handle.LeaseID, handle.OwnerID)
}
