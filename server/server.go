package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/rpc"
	"sort"
	"sync"
	"testetcd/kv"
	"testetcd/raft"
	"testetcd/storage"
	"time"
)

type Server struct {
	mu             sync.Mutex
	shutdownOnce   sync.Once
	cleanupStop    chan struct{}
	cleanupDone    chan struct{}
	applyStop      chan struct{}
	applyDone      chan struct{}
	store          *kv.KVStore
	rf             *raft.Raft
	applyCh        chan raft.ApplyMsg
	id             int
	addr           string
	peers          []string
	wal            *storage.WAL
	snapshot       *storage.Snapshot
	startupErr     error
	persistenceErr error

	replyCh    map[int]chan interface{}
	rpcAddr    string
	httpAddr   string
	watchers   *WatchManager
	leaseMgr   *LeaseManager
	lockMgr    *LockManager
	currentRev int64
}

type serverSnapshot struct {
	Store      json.RawMessage `json:"store"`
	Leases     json.RawMessage `json:"leases"`
	CurrentRev int64           `json:"current_revision"`
}

type applyBarrier struct {
	done chan struct{}
}

type WatchManager struct {
	watchMu        sync.Mutex
	watchers       map[string]map[uint64]*watcherInfo // exact key -> watcherID -> watcherInfo
	prefixWatchers map[string]map[uint64]*watcherInfo // prefix -> watcherID -> watcherInfo
	nextID         uint64
	eventLog       []kv.WatchEvent // 历史事件日志（简单版）
}

type watcherInfo struct {
	id         uint64
	ch         chan Event
	prevValue  string
	persistent bool  // 是否持续监听
	expireAt   int64 // 超时时间戳 (unix nano)
	fromRev    int64 // 从哪个 revision 开始监听
}

type Watcher struct {
	Key    string
	ID     uint64
	Ch     chan Event
	Cancel chan struct{}
}

type Event struct {
	Key      string
	Value    string
	Type     string
	Revision int64
}

type Lease struct {
	ID       int64
	TTL      int64
	expireAt int64
	Keys     map[string]string
}

type leaseItem struct {
	ID       int64
	expireAt int64
}

type LeaseManager struct {
	mu      sync.Mutex
	leases  map[int64]*Lease
	leaseID int64
	heap    []*leaseItem
}

type leaseSnapshot struct {
	LeaseID int64             `json:"lease_id"`
	Leases  []leaseStateEntry `json:"leases"`
}

type leaseStateEntry struct {
	ID       int64             `json:"id"`
	TTL      int64             `json:"ttl"`
	ExpireAt int64             `json:"expire_at"`
	Keys     map[string]string `json:"keys"`
}

func NewLeaseManager() *LeaseManager {
	return &LeaseManager{
		leases: make(map[int64]*Lease),
		heap:   make([]*leaseItem, 0),
	}
}

// Snapshot returns all lease metadata, including absolute expiry timestamps.
func (le *LeaseManager) Snapshot() ([]byte, error) {
	le.mu.Lock()
	defer le.mu.Unlock()

	s := leaseSnapshot{LeaseID: le.leaseID, Leases: make([]leaseStateEntry, 0, len(le.leases))}
	for _, lease := range le.leases {
		keys := make(map[string]string, len(lease.Keys))
		for key, value := range lease.Keys {
			keys[key] = value
		}
		s.Leases = append(s.Leases, leaseStateEntry{
			ID: lease.ID, TTL: lease.TTL, ExpireAt: lease.expireAt, Keys: keys,
		})
	}
	return json.Marshal(s)
}

// Restore replaces lease state and rebuilds the expiry heap.
func (le *LeaseManager) Restore(data []byte) error {
	var s leaseSnapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	leases := make(map[int64]*Lease, len(s.Leases))
	heap := make([]*leaseItem, 0, len(s.Leases))
	maxLeaseID := int64(0)
	for _, entry := range s.Leases {
		if entry.ID <= 0 || entry.TTL <= 0 || entry.ExpireAt <= 0 {
			return fmt.Errorf("invalid lease snapshot entry %d", entry.ID)
		}
		if _, exists := leases[entry.ID]; exists {
			return fmt.Errorf("duplicate lease snapshot entry %d", entry.ID)
		}
		keys := make(map[string]string, len(entry.Keys))
		for key, value := range entry.Keys {
			keys[key] = value
		}
		leases[entry.ID] = &Lease{ID: entry.ID, TTL: entry.TTL, expireAt: entry.ExpireAt, Keys: keys}
		heap = append(heap, &leaseItem{ID: entry.ID, expireAt: entry.ExpireAt})
		if entry.ID > maxLeaseID {
			maxLeaseID = entry.ID
		}
	}
	if s.LeaseID < maxLeaseID {
		return fmt.Errorf("lease counter %d is below existing lease id %d", s.LeaseID, maxLeaseID)
	}

	le.mu.Lock()
	le.leaseID = s.LeaseID
	le.leases = leases
	le.heap = heap
	for i := len(le.heap)/2 - 1; i >= 0; i-- {
		le.bubbleDown(i)
	}
	le.mu.Unlock()
	return nil
}

func (le *LeaseManager) Grant(ttl int64) int64 {
	return le.GrantUntil(ttl, time.Now().UnixNano()+ttl*1e9)
}

func (le *LeaseManager) GrantUntil(ttl, expireAt int64) int64 {
	le.mu.Lock()
	defer le.mu.Unlock()

	le.leaseID++
	id := le.leaseID

	l := &Lease{
		ID:       id,
		TTL:      ttl,
		expireAt: expireAt,
		Keys:     make(map[string]string),
	}
	le.leases[id] = l

	item := &leaseItem{ID: id, expireAt: expireAt}
	le.heap = append(le.heap, item)
	le.bubbleUp(len(le.heap) - 1)

	return id
}

func (le *LeaseManager) Attach(leaseId int64, key string, value string) {
	le.mu.Lock()
	defer le.mu.Unlock()

	lease := le.leases[leaseId]
	if lease == nil {
		return
	}
	lease.Keys[key] = value
}

func (le *LeaseManager) Revoke(key string) int64 {
	le.mu.Lock()
	defer le.mu.Unlock()

	for id, lease := range le.leases {
		if _, ok := lease.Keys[key]; ok {
			delete(lease.Keys, key)
			return id
		}
	}
	return 0
}

func (le *LeaseManager) GetLease(leaseId int64) *Lease {
	le.mu.Lock()
	defer le.mu.Unlock()
	return le.leases[leaseId]
}

func (le *LeaseManager) KeepAlive(leaseId int64, ttl int64) bool {
	return le.KeepAliveUntil(leaseId, ttl, time.Now().UnixNano()+ttl*1e9)
}

func (le *LeaseManager) KeepAliveUntil(leaseId int64, ttl, expireAt int64) bool {
	le.mu.Lock()
	defer le.mu.Unlock()

	lease := le.leases[leaseId]
	if lease == nil {
		return false
	}

	lease.TTL = ttl
	lease.expireAt = expireAt

	for i, item := range le.heap {
		if item.ID == leaseId {
			item.expireAt = lease.expireAt
			le.bubbleDown(i)
			le.bubbleUp(i)
			break
		}
	}
	return true
}

func (le *LeaseManager) RevokeLease(leaseId int64) []string {
	le.mu.Lock()
	defer le.mu.Unlock()
	return le.revokeLeaseLocked(leaseId)
}

// RevokeLeaseIfMatches removes a lease only if it still has the expiry
// observed by the cleanup loop. This prevents a concurrent KeepAlive from
// being undone by a stale expiration command.
func (le *LeaseManager) RevokeLeaseIfMatches(leaseId, expectedExpireAt int64) []string {
	le.mu.Lock()
	defer le.mu.Unlock()

	lease := le.leases[leaseId]
	if lease == nil || lease.expireAt != expectedExpireAt {
		return nil
	}
	return le.revokeLeaseLocked(leaseId)
}

func (le *LeaseManager) revokeLeaseLocked(leaseId int64) []string {

	lease := le.leases[leaseId]
	if lease == nil {
		return nil
	}

	keys := make([]string, 0, len(lease.Keys))
	for key := range lease.Keys {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	delete(le.leases, leaseId)

	for i, item := range le.heap {
		if item.ID == leaseId {
			le.heap[i] = le.heap[len(le.heap)-1]
			le.heap = le.heap[:len(le.heap)-1]
			if i < len(le.heap) {
				le.bubbleDown(i)
				le.bubbleUp(i)
			}
			break
		}
	}

	return keys
}

func (le *LeaseManager) GetExpiringLease() *leaseItem {
	le.mu.Lock()
	defer le.mu.Unlock()

	if len(le.heap) == 0 {
		return nil
	}
	item := *le.heap[0]
	return &item
}

func (le *LeaseManager) RemoveExpiredLease() ([]string, int64) {
	le.mu.Lock()
	defer le.mu.Unlock()

	if len(le.heap) == 0 {
		return nil, 0
	}

	item := le.heap[0]
	now := time.Now().UnixNano()
	if item.expireAt > now {
		return nil, 0
	}

	lease := le.leases[item.ID]
	if lease == nil {
		return nil, 0
	}

	keys := make([]string, 0, len(lease.Keys))
	for key := range lease.Keys {
		keys = append(keys, key)
	}

	delete(le.leases, item.ID)

	le.heap[0] = le.heap[len(le.heap)-1]
	le.heap = le.heap[:len(le.heap)-1]
	if len(le.heap) > 0 {
		le.bubbleDown(0)
	}

	return keys, item.ID
}

// 插入新元素并保证最小的元素在堆顶
func (le *LeaseManager) bubbleUp(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if le.heap[i].expireAt >= le.heap[parent].expireAt {
			break
		}
		le.heap[i], le.heap[parent] = le.heap[parent], le.heap[i]
		i = parent
	}
}

// 删除某个元素后调整堆使得堆最小元素在顶
func (le *LeaseManager) bubbleDown(i int) {
	n := len(le.heap)
	for {
		left := 2*i + 1
		right := 2*i + 2
		smallest := i

		if left < n && le.heap[left].expireAt < le.heap[smallest].expireAt {
			smallest = left
		}
		if right < n && le.heap[right].expireAt < le.heap[smallest].expireAt {
			smallest = right
		}
		if smallest == i {
			break
		}
		le.heap[i], le.heap[smallest] = le.heap[smallest], le.heap[i]
		i = smallest
	}
}

func NewServer(
	id int,
	raftAddr string,
	peers []string,
	wal *storage.WAL,
	snapshot *storage.Snapshot,
	rpcAddr string,
	httpAddr string,
) *Server {
	s := &Server{
		store:       kv.NewKVStore(),
		id:          id,
		addr:        raftAddr,
		peers:       peers,
		applyCh:     make(chan raft.ApplyMsg, 100),
		wal:         wal,
		snapshot:    snapshot,
		replyCh:     make(map[int]chan interface{}),
		rpcAddr:     rpcAddr,
		httpAddr:    httpAddr,
		watchers:    NewWatchManager(),
		leaseMgr:    NewLeaseManager(),
		currentRev:  0,
		cleanupStop: make(chan struct{}),
		cleanupDone: make(chan struct{}),
		applyStop:   make(chan struct{}),
		applyDone:   make(chan struct{}),
	}

	if err := s.restorePersistentState(); err != nil {
		log.Printf("[Server %d] persistent state restore failed: %v", id, err)
		s.startupErr = err
		// Do not start Raft or expose an empty state after a failed restore.
		// Callers can inspect StartupError and fail the process safely.
		return s
	}

	// 初始化分布式锁管理器
	s.lockMgr = NewLockManager(s.leaseMgr, s.store)

	// 创建Raft实例，mock Persister和peers
	persister := raft.MakePersister()
	s.rf = raft.Make(peers, id, persister, s.applyCh)
	// 启动Raft HTTP服务，监听Raft节点间通信端口
	go func() {
		err := s.rf.StartHTTPServer(raftAddr)
		if err != nil {
			log.Fatalf("Raft HTTP server failed: %v", err)
		}
	}()
	// 启动应用日志循环
	go s.applyLoop()

	// 启动过期watcher清理goroutine
	go s.cleanupLoop()

	return s
}

// restorePersistentState loads the application snapshot before Raft starts
// producing apply messages. A clean snapshot is authoritative; its WAL is
// cleared only after the snapshot has been durably written (see Shutdown).
func (s *Server) restorePersistentState() error {
	hasSnapshot := false
	if s.snapshot != nil {
		data, _, _ := s.snapshot.Load()
		if len(data) > 0 {
			var state serverSnapshot
			if err := json.Unmarshal(data, &state); err != nil {
				return fmt.Errorf("decode snapshot: %w", err)
			}
			if len(state.Store) == 0 && len(state.Leases) == 0 {
				return fmt.Errorf("snapshot has no server state")
			}
			if len(state.Store) > 0 {
				if err := s.store.Restore(state.Store); err != nil {
					return fmt.Errorf("restore KV snapshot: %w", err)
				}
			}
			if len(state.Leases) > 0 {
				if err := s.leaseMgr.Restore(state.Leases); err != nil {
					return fmt.Errorf("restore lease snapshot: %w", err)
				}
			}
			s.currentRev = state.CurrentRev
			if storeRev := s.store.GetCurrentRevision(); storeRev > s.currentRev {
				s.currentRev = storeRev
			}
			hasSnapshot = true
		}
	}

	if s.wal == nil {
		return nil
	}
	if hasSnapshot {
		// A snapshot normally follows a WAL clear and therefore has a newer WAL
		// file identity. Matching IDs mean the snapshot was written before the
		// clear and its offset splits the old WAL into included and new records.
		// A different ID means the old WAL was already cleared; records in the
		// replacement file are all newer than the snapshot and replay from zero.
		snapshotWALID := s.snapshot.WALID()
		walID, idErr := s.wal.FileID()
		if idErr != nil {
			return fmt.Errorf("identify WAL during recovery: %w", idErr)
		}
		offset := int64(0)
		if snapshotWALID != "" && snapshotWALID == walID {
			offset = s.snapshot.WALOffset()
		} else if size, err := s.wal.Size(); err == nil && size > 0 {
			log.Printf("[Server %d] WAL is newer than snapshot; replaying from start", s.id)
		}
		entries, err := s.wal.ReadEntriesFrom(offset)
		if err != nil {
			return fmt.Errorf("read WAL suffix: %w", err)
		}
		return s.replayEntries(entries, "WAL suffix")
	}
	entries, err := s.wal.ReadEntries()
	if err != nil {
		return fmt.Errorf("read WAL: %w", err)
	}
	return s.replayEntries(entries, "WAL")
}

func (s *Server) replayEntries(entries []interface{}, source string) error {
	for i, entry := range entries {
		cmd, ok := commandFromValue(entry)
		if !ok {
			return fmt.Errorf("decode %s entry %d", source, i)
		}
		if err := s.applyRecoveredCommand(cmd); err != nil {
			return fmt.Errorf("replay %s entry %d: %w", source, i, err)
		}
	}
	return nil
}

func commandFromValue(value interface{}) (kv.Command, bool) {
	if cmd, ok := value.(kv.Command); ok {
		return cmd, true
	}
	m, ok := value.(map[string]interface{})
	if !ok {
		return kv.Command{}, false
	}
	return kv.Command{
		Type:        kv.CommandType(toInt(m["Type"])),
		Key:         toString(m["Key"]),
		Value:       toString(m["Value"]),
		LeaseID:     toInt64(m["LeaseID"]),
		TTL:         toInt64(m["TTL"]),
		ExpireAt:    toInt64(m["ExpireAt"]),
		Revision:    toInt64(m["Revision"]),
		Tombstone:   toBool(m["Tombstone"]),
		ExpectedRev: toInt64(m["ExpectedRev"]),
		Prefix:      toString(m["Prefix"]),
	}, true
}

// applyRecoveredCommand replays a committed command without Raft, WAL, reply
// channels, or watchers. It intentionally mirrors the state-machine effects
// in applyLoop and is only called during single-threaded startup recovery.
func (s *Server) applyRecoveredCommand(cmd kv.Command) error {
	if cmd.Type == kv.CmdPut || cmd.Type == kv.CmdDelete || cmd.Type == kv.CmdCAS || cmd.Type == kv.CmdLeaseAttach || cmd.Type == kv.CmdDeletePrefix {
		if cmd.Revision == 0 {
			s.currentRev++
			cmd.Revision = s.currentRev
		} else if cmd.Revision > s.currentRev {
			s.currentRev = cmd.Revision
		}
	}

	switch cmd.Type {
	case kv.CmdLeaseGrant:
		ttl := cmd.TTL
		if ttl <= 0 {
			ttl = 10
		}
		expireAt := cmd.ExpireAt
		if expireAt <= 0 {
			expireAt = time.Now().UnixNano() + ttl*1e9
		}
		s.leaseMgr.GrantUntil(ttl, expireAt)
	case kv.CmdLeaseRevoke:
		var keys []string
		if cmd.ExpireAt > 0 {
			keys = s.leaseMgr.RevokeLeaseIfMatches(cmd.LeaseID, cmd.ExpireAt)
		} else {
			keys = s.leaseMgr.RevokeLease(cmd.LeaseID)
		}
		for _, key := range keys {
			s.currentRev++
			if _, err := s.store.Apply(kv.Command{Type: kv.CmdDelete, Key: key, Revision: s.currentRev}); err != nil {
				return err
			}
		}
	case kv.CmdLeaseKeepAlive:
		ttl := cmd.TTL
		if ttl <= 0 {
			ttl = 10
		}
		expireAt := cmd.ExpireAt
		if expireAt <= 0 {
			expireAt = time.Now().UnixNano() + ttl*1e9
		}
		s.leaseMgr.KeepAliveUntil(cmd.LeaseID, ttl, expireAt)
	case kv.CmdLeaseAttach:
		if cmd.LeaseID > 0 {
			s.leaseMgr.Revoke(cmd.Key)
			s.leaseMgr.Attach(cmd.LeaseID, cmd.Key, cmd.Value)
			s.store.SetKeyLease(cmd.Key, cmd.LeaseID)
			cmd.Type = kv.CmdPut
			if _, err := s.store.Apply(cmd); err != nil {
				return err
			}
		}
	case kv.CmdDeletePrefix:
		deletedKeys := s.store.DeletePrefix(cmd.Prefix)
		for _, key := range deletedKeys {
			s.leaseMgr.Revoke(key)
		}
	case kv.CmdGetPrefix:
		// Read-only commands are journaled with the applied command stream but
		// do not alter state during recovery.
	case kv.CmdCAS:
		leaseID := s.store.GetKeyLease(cmd.Key)
		result, err := s.store.Apply(cmd)
		if err != nil {
			return err
		}
		if result == "OK" && leaseID > 0 {
			s.leaseMgr.Attach(leaseID, cmd.Key, cmd.Value)
		}
	default:
		if cmd.Type == kv.CmdPut || cmd.Type == kv.CmdDelete {
			s.leaseMgr.Revoke(cmd.Key)
		}
		if _, err := s.store.Apply(cmd); err != nil {
			return err
		}
		if cmd.Type == kv.CmdPut && cmd.LeaseID > 0 {
			s.leaseMgr.Attach(cmd.LeaseID, cmd.Key, cmd.Value)
			s.store.SetKeyLease(cmd.Key, cmd.LeaseID)
		}
	}
	if storeRev := s.store.GetCurrentRevision(); storeRev > s.currentRev {
		s.currentRev = storeRev
	}
	return nil
}

func (s *Server) savePersistentState() error {
	if s.snapshot == nil {
		return nil
	}
	storeData, err := s.store.Snapshot()
	if err != nil {
		return fmt.Errorf("encode KV snapshot: %w", err)
	}
	leaseData, err := s.leaseMgr.Snapshot()
	if err != nil {
		return fmt.Errorf("encode lease snapshot: %w", err)
	}
	currentRev := s.currentRev
	if storeRev := s.store.GetCurrentRevision(); storeRev > currentRev {
		currentRev = storeRev
	}
	data, err := json.Marshal(serverSnapshot{
		Store: storeData, Leases: leaseData, CurrentRev: currentRev,
	})
	if err != nil {
		return fmt.Errorf("encode server snapshot: %w", err)
	}
	walID := ""
	walOffset := int64(0)
	if s.wal != nil {
		walID, err = s.wal.FileID()
		if err != nil {
			return fmt.Errorf("identify WAL before snapshot: %w", err)
		}
		walOffset, err = s.wal.Size()
		if err != nil {
			return fmt.Errorf("measure WAL before snapshot: %w", err)
		}
	}
	if err := s.snapshot.SaveWithWAL(data, 0, int(currentRev), walID, walOffset); err != nil {
		return fmt.Errorf("write server snapshot: %w", err)
	}
	if s.wal != nil {
		if err := s.wal.Clear(); err != nil {
			return fmt.Errorf("clear WAL after snapshot: %w", err)
		}
	}
	return nil
}

func NewWatchManager() *WatchManager {
	return &WatchManager{
		watchers:       make(map[string]map[uint64]*watcherInfo),
		prefixWatchers: make(map[string]map[uint64]*watcherInfo),
	}
}

// 添加普通 watcher
func (wa *WatchManager) AddWatcher(key string, persistent bool, timeout time.Duration, fromRev int64) *Watcher {
	return wa.addWatcherInternal(key, "", persistent, timeout, fromRev)
}

// 添加 prefix watcher
func (wa *WatchManager) AddPrefixWatcher(prefix string, persistent bool, timeout time.Duration, fromRev int64) *Watcher {
	return wa.addWatcherInternal("", prefix, persistent, timeout, fromRev)
}

func (wa *WatchManager) addWatcherInternal(key string, prefix string, persistent bool, timeout time.Duration, fromRev int64) *Watcher {
	wa.watchMu.Lock()
	defer wa.watchMu.Unlock()

	wa.nextID++
	id := wa.nextID

	ch := make(chan Event, 100)
	cancelCh := make(chan struct{})

	expireAt := int64(0)
	if timeout > 0 {
		expireAt = time.Now().Add(timeout).UnixNano()
	}

	info := &watcherInfo{
		id:         id,
		ch:         ch,
		persistent: persistent,
		expireAt:   expireAt,
		fromRev:    fromRev,
	}

	if prefix != "" {
		if wa.prefixWatchers[prefix] == nil {
			wa.prefixWatchers[prefix] = make(map[uint64]*watcherInfo)
		}
		wa.prefixWatchers[prefix][id] = info
	} else {
		if wa.watchers[key] == nil {
			wa.watchers[key] = make(map[uint64]*watcherInfo)
		}
		wa.watchers[key][id] = info
	}

	return &Watcher{
		Key:    key,
		ID:     id,
		Ch:     ch,
		Cancel: cancelCh,
	}
}

// 取消watcher
func (wa *WatchManager) RemoveWatcher(key string, id uint64) {
	wa.watchMu.Lock()
	defer wa.watchMu.Unlock()
	if watchers, ok := wa.watchers[key]; ok {
		if info, ok := watchers[id]; ok {
			close(info.ch)
			delete(watchers, id)
		}
	}
}

// 取消 prefix watcher
func (wa *WatchManager) RemovePrefixWatcher(prefix string, id uint64) {
	wa.watchMu.Lock()
	defer wa.watchMu.Unlock()
	if watchers, ok := wa.prefixWatchers[prefix]; ok {
		if info, ok := watchers[id]; ok {
			close(info.ch)
			delete(watchers, id)
		}
	}
}

// 清理过期的watcher
func (wa *WatchManager) CleanupExpired() {
	wa.watchMu.Lock()
	defer wa.watchMu.Unlock()

	now := time.Now().UnixNano()
	for key, watchers := range wa.watchers {
		for id, info := range watchers {
			if info.expireAt > 0 && now > info.expireAt {
				close(info.ch)
				delete(watchers, id)
			}
		}
		if len(watchers) == 0 {
			delete(wa.watchers, key)
		}
	}
}

func (s *Server) notifyWatchers(cmd kv.Command) {
	var eventType string
	switch cmd.Type {
	case kv.CmdPut:
		eventType = "PUT"
	case kv.CmdDelete, kv.CmdDeletePrefix:
		eventType = "DELETE"
	default:
		return
	}

	event := Event{
		Key:      cmd.Key,
		Value:    cmd.Value,
		Type:     eventType,
		Revision: cmd.Revision,
	}

	watchEvent := kv.WatchEvent{
		Key:      cmd.Key,
		Value:    cmd.Value,
		Type:     eventType,
		Revision: cmd.Revision,
	}
	s.watchers.watchMu.Lock()
	s.watchers.eventLog = append(s.watchers.eventLog, watchEvent)

	s.notifyExactWatchers(cmd.Key, event)
	s.notifyPrefixWatchers(cmd.Key, event)

	s.watchers.watchMu.Unlock()
}

func (s *Server) notifyExactWatchers(key string, event Event) {
	watchers, ok := s.watchers.watchers[key]
	if !ok {
		return
	}

	for id, info := range watchers {
		if info.fromRev > 0 {
			for _, ev := range s.watchers.eventLog {
				if ev.Key == key && ev.Revision >= info.fromRev && ev.Revision < event.Revision {
					histEvent := Event{
						Key:   ev.Key,
						Value: ev.Value,
						Type:  ev.Type,
					}
					select {
					case info.ch <- histEvent:
					default:
					}
				}
			}
		}

		select {
		case info.ch <- event:
			if !info.persistent {
				close(info.ch)
				delete(watchers, id)
			}
		default:
		}
	}

	if len(watchers) == 0 {
		delete(s.watchers.watchers, key)
	}
}

func (s *Server) notifyPrefixWatchers(key string, event Event) {
	for prefix, watchers := range s.watchers.prefixWatchers {
		if len(prefix) == 0 || (len(key) >= len(prefix) && key[:len(prefix)] == prefix) {
			for id, info := range watchers {
				if info.fromRev > 0 {
					for _, ev := range s.watchers.eventLog {
						if ev.Revision >= info.fromRev && ev.Revision < event.Revision {
							prefixMatch := len(prefix) == 0 || (len(ev.Key) >= len(prefix) && ev.Key[:len(prefix)] == prefix)
							if prefixMatch {
								histEvent := Event{
									Key:   ev.Key,
									Value: ev.Value,
									Type:  ev.Type,
								}
								select {
								case info.ch <- histEvent:
								default:
								}
							}
						}
					}
				}

				select {
				case info.ch <- event:
					if !info.persistent {
						close(info.ch)
						delete(watchers, id)
					}
				default:
				}
			}

			if len(watchers) == 0 {
				delete(s.watchers.prefixWatchers, prefix)
			}
		}
	}
}

func (s *Server) CancelWatcher(key string, id uint64) {
	s.watchers.RemoveWatcher(key, id)
}

func (s *Server) cleanupLoop() {
	defer close(s.cleanupDone)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.cleanupExpiredLeases()
			s.watchers.CleanupExpired()
		case <-s.cleanupStop:
			return
		}
	}
}

func (s *Server) cleanupExpiredLeases() {
	for {
		item := s.leaseMgr.GetExpiringLease()
		if item == nil || item.expireAt > time.Now().UnixNano() {
			return
		}
		// Expiration is a state-machine command so every replica writes the
		// same revoke to its WAL and assigns the same deletion revisions.
		if _, err := s.Submit(kv.Command{Type: kv.CmdLeaseRevoke, LeaseID: item.ID, ExpireAt: item.expireAt}); err != nil {
			return
		}
	}
}

// 启动应用日志循环
func (s *Server) applyLoop() {
	defer close(s.applyDone)
	for {
		var msg raft.ApplyMsg
		select {
		case <-s.applyStop:
			return
		case msg = <-s.applyCh:
		}
		if barrier, ok := msg.Command.(applyBarrier); ok {
			close(barrier.done)
			continue
		}
		if !msg.CommandValid {
			continue
		}
		var result string
		var cmdIdx int

		s.mu.Lock()
		cmd, ok := msg.Command.(kv.Command)
		if !ok {
			if m, isMap := msg.Command.(map[string]interface{}); isMap {
				cmd = kv.Command{
					Key:         toString(m["Key"]),
					Value:       toString(m["Value"]),
					LeaseID:     toInt64(m["LeaseID"]),
					TTL:         toInt64(m["TTL"]),
					ExpireAt:    toInt64(m["ExpireAt"]),
					Type:        kv.CommandType(toInt(m["Type"])),
					Revision:    toInt64(m["Revision"]),
					Tombstone:   toBool(m["Tombstone"]),
					ExpectedRev: toInt64(m["ExpectedRev"]),
					Prefix:      toString(m["Prefix"]),
				}
				ok = true
			}
		}
		if !ok {
			s.mu.Unlock()
			continue
		}

		// 增加全局 revision 并设置到命令中（写操作）
		if cmd.Type == kv.CmdPut || cmd.Type == kv.CmdDelete || cmd.Type == kv.CmdCAS || cmd.Type == kv.CmdLeaseAttach {
			s.currentRev++
			cmd.Revision = s.currentRev
		}

		// CmdDeletePrefix 需要单独处理 revision
		if cmd.Type == kv.CmdDeletePrefix {
			s.currentRev++
			cmd.Revision = s.currentRev
		}

		if cmd.Type == kv.CmdLeaseGrant || cmd.Type == kv.CmdLeaseKeepAlive {
			ttl := cmd.TTL
			if ttl <= 0 {
				ttl = 10
			}
			if cmd.ExpireAt <= 0 {
				cmd.ExpireAt = time.Now().UnixNano() + ttl*1e9
			}
		}

		if s.persistenceErr != nil {
			cmdIdx = msg.CommandIndex
			failure := s.persistenceErr
			s.mu.Unlock()
			s.notifyReply(cmdIdx, failure)
			continue
		}
		if s.wal != nil {
			if err := s.wal.WriteEntry(cmd); err != nil {
				s.persistenceErr = fmt.Errorf("write WAL: %w", err)
				cmdIdx = msg.CommandIndex
				failure := s.persistenceErr
				s.mu.Unlock()
				s.notifyReply(cmdIdx, failure)
				continue
			}
		}

		switch cmd.Type {
		case kv.CmdLeaseGrant:
			ttl := cmd.TTL
			if ttl <= 0 {
				ttl = 10
			}
			leaseId := s.leaseMgr.GrantUntil(ttl, cmd.ExpireAt)
			result = fmt.Sprintf("%d", leaseId)

		case kv.CmdLeaseRevoke:
			leaseId := cmd.LeaseID
			if leaseId > 0 {
				var keys []string
				if cmd.ExpireAt > 0 {
					keys = s.leaseMgr.RevokeLeaseIfMatches(leaseId, cmd.ExpireAt)
				} else {
					keys = s.leaseMgr.RevokeLease(leaseId)
				}
				for _, key := range keys {
					s.currentRev++
					deleteCmd := kv.Command{Type: kv.CmdDelete, Key: key, Revision: s.currentRev}
					s.store.Apply(deleteCmd)
					s.notifyWatchers(deleteCmd)
				}
			}
			result = "OK"

		case kv.CmdLeaseKeepAlive:
			leaseId := cmd.LeaseID
			ttl := cmd.TTL
			if ttl <= 0 {
				ttl = 10
			}
			ok := s.leaseMgr.KeepAliveUntil(leaseId, ttl, cmd.ExpireAt)
			if ok {
				result = "OK"
			} else {
				result = "ERROR"
			}

		case kv.CmdLeaseAttach:
			if cmd.LeaseID > 0 {
				s.leaseMgr.Revoke(cmd.Key)
				s.leaseMgr.Attach(cmd.LeaseID, cmd.Key, cmd.Value)
				s.store.SetKeyLease(cmd.Key, cmd.LeaseID)
				// 使用 Apply 来确保 MVCC 正确记录版本
				cmd.Type = kv.CmdPut
				result, _ = s.store.Apply(cmd)
			}
			result = "OK"
			//
		case kv.CmdCAS:
			leaseID := s.store.GetKeyLease(cmd.Key)
			result, _ = s.store.Apply(cmd)
			if result == "OK" {
				if leaseID > 0 {
					s.leaseMgr.Attach(leaseID, cmd.Key, cmd.Value)
				}
				cmd.Type = kv.CmdPut
				s.notifyWatchers(cmd)
			}

		case kv.CmdDeletePrefix:
			deletedKeys := s.store.DeletePrefix(cmd.Prefix)
			for _, key := range deletedKeys {
				s.leaseMgr.Revoke(key)
			}
			if storeRev := s.store.GetCurrentRevision(); storeRev > s.currentRev {
				s.currentRev = storeRev
			}
			for _, key := range deletedKeys {
				deleteCmd := kv.Command{Type: kv.CmdDelete, Key: key, Revision: s.currentRev}
				s.notifyWatchers(deleteCmd)
			}
			result = fmt.Sprintf("%d", len(deletedKeys))
			cmdIdx = msg.CommandIndex
			s.notifyReply(cmdIdx, result)
			s.mu.Unlock()
			s.mu.Lock()
			if msg.CommandIndex > s.rf.LastApplied {
				s.rf.LastApplied = msg.CommandIndex
			}
			s.mu.Unlock()
			continue

		case kv.CmdGetPrefix:
			resultMap := s.store.GetPrefix(cmd.Prefix)
			result = fmt.Sprintf("%v", resultMap)
			cmdIdx = msg.CommandIndex
			s.notifyReply(cmdIdx, result)
			s.mu.Unlock()
			s.mu.Lock()
			if msg.CommandIndex > s.rf.LastApplied {
				s.rf.LastApplied = msg.CommandIndex
			}
			s.mu.Unlock()
			continue

		case kv.CmdGet:
			result, _ = s.store.Apply(cmd)
			cmdIdx = msg.CommandIndex
			s.notifyReply(cmdIdx, result)
			s.mu.Unlock()
			s.mu.Lock()
			if msg.CommandIndex > s.rf.LastApplied {
				s.rf.LastApplied = msg.CommandIndex
			}
			s.mu.Unlock()
			continue

		default:
			if cmd.Type == kv.CmdPut || cmd.Type == kv.CmdDelete {
				s.leaseMgr.Revoke(cmd.Key)
			}
			result, _ = s.store.Apply(cmd)
			// 自动关联 LeaseID
			if cmd.LeaseID > 0 && cmd.Type == kv.CmdPut {
				s.leaseMgr.Attach(cmd.LeaseID, cmd.Key, cmd.Value)
				s.store.SetKeyLease(cmd.Key, cmd.LeaseID)
			}
			s.notifyWatchers(cmd)
		}

		cmdIdx = msg.CommandIndex
		s.mu.Unlock()

		if cmdIdx >= 0 {
			s.notifyReply(cmdIdx, result)
		}

		s.mu.Lock()
		if msg.CommandIndex > s.rf.LastApplied {
			s.rf.LastApplied = msg.CommandIndex
		}
		s.mu.Unlock()
	}
}

func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toInt64(v interface{}) int64 {
	switch val := v.(type) {
	case int64:
		return val
	case float64: // JSON numbers decode as float64
		return int64(val)
	case int:
		return int64(val)
	}
	return 0
}

func toInt(v interface{}) int {
	switch val := v.(type) {
	case int:
		return val
	case float64:
		return int(val)
	case int64:
		return int(val)
	}
	return 0
}

func toBool(v interface{}) bool {
	switch val := v.(type) {
	case bool:
		return val
	case float64:
		return val != 0
	case int:
		return val != 0
	case int64:
		return val != 0
	}
	return false
}

func (s *Server) notifyReply(cmdIdx int, result interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.replyCh[cmdIdx]; ok {
		select {
		case ch <- result:
		default:
		}
		delete(s.replyCh, cmdIdx)
	}
}

// 向Raft提交命令
func (s *Server) Submit(cmd kv.Command) (string, error) {
	s.mu.Lock()
	if s.startupErr != nil {
		err := s.startupErr
		s.mu.Unlock()
		return "", fmt.Errorf("server unavailable: %w", err)
	}
	if s.persistenceErr != nil {
		err := s.persistenceErr
		s.mu.Unlock()
		return "", fmt.Errorf("server unavailable: %w", err)
	}
	if s.rf == nil {
		s.mu.Unlock()
		return "", fmt.Errorf("server unavailable")
	}
	if !s.IsLeader() {
		s.mu.Unlock()
		return "", fmt.Errorf("not leader")
	}
	if cmd.Type == kv.CmdLeaseGrant || cmd.Type == kv.CmdLeaseKeepAlive {
		ttl := cmd.TTL
		if ttl <= 0 {
			ttl = 10
			cmd.TTL = ttl
		}
		if cmd.ExpireAt <= 0 {
			cmd.ExpireAt = time.Now().UnixNano() + ttl*1e9
		}
	}
	replyCh := make(chan interface{}, 1)

	idx, term, ok := s.rf.Start(cmd)
	if !ok {
		s.mu.Unlock()
		return "", fmt.Errorf("submit failed")
	}

	log.Printf("[Server %d] Submit command at index %d, term %d", s.id, idx, term)

	s.replyCh[idx] = replyCh
	s.mu.Unlock()

	select {
	case result := <-replyCh:
		switch value := result.(type) {
		case string:
			return value, nil
		case error:
			return "", value
		default:
			return "", fmt.Errorf("unexpected command result %T", result)
		}
	case <-time.After(5 * time.Second):
		s.mu.Lock()
		delete(s.replyCh, idx)
		s.mu.Unlock()
		return "", fmt.Errorf("timeout")
	}
}

// 从本地存储读取（无需经过Raft）
func (s *Server) Get(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.Get(key)
}

// GetPrefix 本地读取（无需经过 Raft）
func (s *Server) GetPrefix(prefix string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.GetPrefix(prefix)
}

// Leader检测
func (s *Server) IsLeader() bool {
	return s.rf != nil && s.rf.State == raft.Leader
}

// StartupError reports a persistence recovery error that prevented the
// server from starting its Raft and service goroutines.
func (s *Server) StartupError() error {
	return s.startupErr
}

// 启动RPC服务器
func (s *Server) StartRPCServer() {
	if s.startupErr != nil {
		log.Printf("[Server %d] RPC server not started: %v", s.id, s.startupErr)
		return
	}
	rpc.Register(NewHandler(s))
	listener, err := net.Listen("tcp", s.rpcAddr)
	if err != nil {
		log.Fatal("RPC listen error:", err)
	}
	go rpc.Accept(listener)
}

// 启动HTTP服务器
func (s *Server) StartHTTPServer() error {
	if s.startupErr != nil {
		return fmt.Errorf("server unavailable: %w", s.startupErr)
	}
	mux := http.NewServeMux()
	handler := NewHTTPHandler(s)
	mux.HandleFunc("/put", handler.Put)
	mux.HandleFunc("/get", handler.Get)
	mux.HandleFunc("/delete", handler.Delete)
	mux.HandleFunc("/watch", handler.Watch)
	mux.HandleFunc("/wait", handler.Wait)
	mux.HandleFunc("/health", handler.Health)
	mux.HandleFunc("/stats", handler.Stats)
	mux.HandleFunc("/lease/grant", handler.LeaseGrant)
	mux.HandleFunc("/lease/revoke", handler.LeaseRevoke)
	mux.HandleFunc("/lease/keepalive", handler.LeaseKeepAlive)
	mux.HandleFunc("/lease/attach", handler.LeaseAttach)
	mux.HandleFunc("/lock/acquire", handler.LockAcquire)
	mux.HandleFunc("/lock/release", handler.LockRelease)
	mux.HandleFunc("/lock/keepalive", handler.LockKeepAlive)
	mux.HandleFunc("/lock/status", handler.LockStatus)
	mux.HandleFunc("/watch/prefix", handler.WatchPrefix)
	mux.HandleFunc("/delete/prefix", handler.DeletePrefix)
	mux.HandleFunc("/get/prefix", handler.GetPrefix)
	return http.ListenAndServe(s.httpAddr, mux)
}

// 获取服务器统计信息
func (s *Server) GetStats() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	stats := map[string]interface{}{
		"id":         s.id,
		"is_leader":  s.IsLeader(),
		"store_size": s.store.Size(),
	}
	if s.rf != nil {
		stats["commit_index"] = s.rf.CommitIndex
		stats["last_applied"] = s.rf.LastApplied
		stats["current_term"] = s.rf.CurrentTerm
	} else {
		stats["commit_index"] = -1
		stats["last_applied"] = -1
		stats["current_term"] = 0
	}
	return stats
}

// 关闭服务器
func (s *Server) Shutdown() {
	s.shutdownOnce.Do(func() {
		if s.rf != nil {
			s.rf.Kill()
			// Stop accepting new committed messages, then let the service-facing
			// applier finish messages already emitted by Raft.
			s.rf.WaitForApplyExit()
			barrier := make(chan struct{})
			s.applyCh <- raft.ApplyMsg{Command: applyBarrier{done: barrier}}
			<-barrier
			close(s.applyStop)
			<-s.applyDone
			close(s.cleanupStop)
			<-s.cleanupDone
		}
		s.mu.Lock()
		if s.startupErr == nil && s.persistenceErr == nil {
			if err := s.savePersistentState(); err != nil {
				log.Printf("[Server %d] failed to save persistent state: %v", s.id, err)
			}
		}
		s.mu.Unlock()
		if s.wal != nil {
			_ = s.wal.Close()
		}
		if s.snapshot != nil {
			_ = s.snapshot.Close()
		}
	})
}
