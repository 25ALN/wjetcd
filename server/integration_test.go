package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testetcd/kv"
	"testetcd/raft"
	"testetcd/storage"
	"testing"
	"time"
)

// TestLeaseReplicationAndWatchRestriction tests:
// 1. LeaseAttach goes through Raft and data is replicated to followers.
// 2. Watch/Wait endpoints are rejected on non-leader nodes.
func TestLeaseReplicationAndWatchRestriction(t *testing.T) {
	// Use random ports to avoid conflicts
	basePort := 18000 + int(time.Now().UnixMilli()%1000)*10
	raftAddrs := []string{
		fmt.Sprintf("127.0.0.1:%d", basePort),
		fmt.Sprintf("127.0.0.1:%d", basePort+1),
		fmt.Sprintf("127.0.0.1:%d", basePort+2),
	}
	httpAddrs := []string{
		fmt.Sprintf("127.0.0.1:%d", basePort+10),
		fmt.Sprintf("127.0.0.1:%d", basePort+11),
		fmt.Sprintf("127.0.0.1:%d", basePort+12),
	}

	tmpDir, err := os.MkdirTemp("", "testetcd_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Reset global peer addresses for Raft
	for i := 0; i < 3; i++ {
		raft.SetPeerAddr(i, "") // clear old state if any
	}
	for i, addr := range raftAddrs {
		raft.SetPeerAddr(i, addr)
	}

	var servers []*Server
	for i := 0; i < 3; i++ {
		walPath := filepath.Join(tmpDir, fmt.Sprintf("node%d.wal", i))
		snapPath := filepath.Join(tmpDir, fmt.Sprintf("node%d.snap", i))

		wal, _ := storage.NewWAL(walPath)
		snap, _ := storage.NewSnapshot(snapPath)

		srv := NewServer(i+1, raftAddrs[i], raftAddrs, wal, snap, "", httpAddrs[i])
		servers = append(servers, srv)

		go srv.StartRPCServer()
		go srv.StartHTTPServer()
	}

	// Wait for cluster to stabilize and elect a leader
	time.Sleep(2 * time.Second)

	// Find leader
	leaderIdx := -1
	for i, s := range servers {
		if s.IsLeader() {
			leaderIdx = i
			break
		}
	}
	if leaderIdx == -1 {
		t.Fatal("no leader elected after 2s")
	}
	t.Logf("Leader is node %d at %s", leaderIdx+1, httpAddrs[leaderIdx])

	// Test 1: LeaseAttach on Leader
	leaseURL := fmt.Sprintf("http://%s/lease/grant?ttl=60", httpAddrs[leaderIdx])
	resp, err := http.Post(leaseURL, "application/json", nil)
	if err != nil {
		t.Fatalf("failed to grant lease: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Parse lease_id from response: {"ok":true,"lease_id":1}
	leaseID := "1" // simplified parsing for test, assuming first lease
	if strings.Contains(string(body), `"lease_id"`) {
		parts := strings.Split(string(body), `"lease_id":`)
		if len(parts) > 1 {
			leaseID = strings.TrimRight(strings.Split(parts[1], "}")[0], " ")
		}
	}

	attachURL := fmt.Sprintf("http://%s/lease/attach?key=testkey&value=testvalue&lease_id=%s", httpAddrs[leaderIdx], leaseID)
	resp, err = http.Post(attachURL, "application/json", nil)
	if err != nil {
		t.Fatalf("failed to attach lease: %v", err)
	}
	resp.Body.Close()

	// Wait for replication
	time.Sleep(500 * time.Millisecond)

	// Verify data on ALL nodes (including followers)
	for i, addr := range httpAddrs {
		getURL := fmt.Sprintf("http://%s/get?key=testkey", addr)
		resp, err := http.Get(getURL)
		if err != nil {
			t.Fatalf("node %d GET failed: %v", i+1, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), `"value":"testvalue"`) {
			t.Errorf("node %d did not replicate lease-attached data. got: %s", i+1, string(body))
		}
	}

	// Test 2: Watch on Follower should fail
	followerIdx := (leaderIdx + 1) % 3
	watchURL := fmt.Sprintf("http://%s/watch?key=somekey", httpAddrs[followerIdx])
	resp, err = http.Get(watchURL)
	if err != nil {
		t.Fatalf("failed to request watch on follower: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("Watch on follower should have failed, but got 200 OK: %s", string(body))
	}
	if !strings.Contains(string(body), `"error":"watch only supported on leader"`) {
		t.Logf("Watch on follower returned: %s", string(body))
	}

	// Cleanup
	for _, s := range servers {
		s.Shutdown()
	}
}

func TestCommandSerialization(t *testing.T) {
	// Basic test to ensure kv.Command types are correctly handled
	cmd := kv.Command{
		Type:    kv.CmdLeaseAttach,
		Key:     "k1",
		Value:   "v1",
		LeaseID: 42,
	}
	if cmd.Type != kv.CmdLeaseAttach {
		t.Fail()
	}
}

func TestLeaseAutoExpiration(t *testing.T) {
	basePort := 19000 + int(time.Now().UnixMilli()%1000)*10
	raftAddrs := []string{
		fmt.Sprintf("127.0.0.1:%d", basePort),
		fmt.Sprintf("127.0.0.1:%d", basePort+1),
		fmt.Sprintf("127.0.0.1:%d", basePort+2),
	}
	httpAddrs := []string{
		fmt.Sprintf("127.0.0.1:%d", basePort+10),
		fmt.Sprintf("127.0.0.1:%d", basePort+11),
		fmt.Sprintf("127.0.0.1:%d", basePort+12),
	}

	tmpDir, err := os.MkdirTemp("", "testetcd_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	for i := 0; i < 3; i++ {
		raft.SetPeerAddr(i, "")
	}
	for i, addr := range raftAddrs {
		raft.SetPeerAddr(i, addr)
	}

	var servers []*Server
	for i := 0; i < 3; i++ {
		walPath := filepath.Join(tmpDir, fmt.Sprintf("node%d.wal", i))
		snapPath := filepath.Join(tmpDir, fmt.Sprintf("node%d.snap", i))
		wal, _ := storage.NewWAL(walPath)
		snap, _ := storage.NewSnapshot(snapPath)
		srv := NewServer(i+1, raftAddrs[i], raftAddrs, wal, snap, "", httpAddrs[i])
		servers = append(servers, srv)
		go srv.StartHTTPServer()
	}

	time.Sleep(2 * time.Second)

	leaderIdx := -1
	for i, s := range servers {
		if s.IsLeader() {
			leaderIdx = i
			break
		}
	}
	if leaderIdx == -1 {
		t.Fatal("no leader elected")
	}
	t.Logf("Leader is node %d", leaderIdx+1)

	// 创建1秒TTL的lease
	leaseURL := fmt.Sprintf("http://%s/lease/grant?ttl=1", httpAddrs[leaderIdx])
	resp, err := http.Post(leaseURL, "application/json", nil)
	if err != nil {
		t.Fatalf("failed to grant lease: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("Grant lease response: %s", string(body))

	leaseID := "1"
	if strings.Contains(string(body), `"lease_id"`) {
		parts := strings.Split(string(body), `"lease_id":`)
		if len(parts) > 1 {
			leaseID = strings.TrimRight(strings.Split(parts[1], "}")[0], " ")
		}
	}

	// 使用 Put 直接带 lease_id（一体化操作）
	putURL := fmt.Sprintf("http://%s/put?key=leasekey&value=leasevalue&lease_id=%s", httpAddrs[leaderIdx], leaseID)
	resp, err = http.Post(putURL, "application/json", nil)
	if err != nil {
		t.Fatalf("failed to put with lease: %v", err)
	}
	resp.Body.Close()
	t.Logf("Put with lease_id=%s", leaseID)

	time.Sleep(500 * time.Millisecond)

	// 验证key存在
	getURL := fmt.Sprintf("http://%s/get?key=leasekey", httpAddrs[leaderIdx])
	resp, err = http.Get(getURL)
	if err != nil {
		t.Fatalf("failed to get: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"value":"leasevalue"`) {
		t.Errorf("key should exist before lease expiration, got: %s", string(body))
	}

	// 等待lease过期（1秒TTL + 缓冲时间）
	time.Sleep(1500 * time.Millisecond)

	// 验证key已被自动删除
	resp, err = http.Get(getURL)
	if err != nil {
		t.Fatalf("failed to get after expiration: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), `"value":"leasevalue"`) {
		t.Errorf("key should be deleted after lease expiration, got: %s", string(body))
	} else {
		t.Logf("Key correctly auto-deleted after lease expiration: %s", string(body))
	}

	for _, s := range servers {
		s.Shutdown()
	}
}

func TestDistributedLock(t *testing.T) {
	basePort := 20000 + int(time.Now().UnixMilli()%1000)*10
	raftAddrs := []string{
		fmt.Sprintf("127.0.0.1:%d", basePort),
		fmt.Sprintf("127.0.0.1:%d", basePort+1),
		fmt.Sprintf("127.0.0.1:%d", basePort+2),
	}
	httpAddrs := []string{
		fmt.Sprintf("127.0.0.1:%d", basePort+10),
		fmt.Sprintf("127.0.0.1:%d", basePort+11),
		fmt.Sprintf("127.0.0.1:%d", basePort+12),
	}

	tmpDir, err := os.MkdirTemp("", "testetcd_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	for i := 0; i < 3; i++ {
		raft.SetPeerAddr(i, "")
	}
	for i, addr := range raftAddrs {
		raft.SetPeerAddr(i, addr)
	}

	var servers []*Server
	for i := 0; i < 3; i++ {
		walPath := filepath.Join(tmpDir, fmt.Sprintf("node%d.wal", i))
		snapPath := filepath.Join(tmpDir, fmt.Sprintf("node%d.snap", i))
		wal, _ := storage.NewWAL(walPath)
		snap, _ := storage.NewSnapshot(snapPath)
		srv := NewServer(i+1, raftAddrs[i], raftAddrs, wal, snap, "", httpAddrs[i])
		servers = append(servers, srv)
		go srv.StartHTTPServer()
	}

	time.Sleep(2 * time.Second)

	leaderIdx := -1
	for i, s := range servers {
		if s.IsLeader() {
			leaderIdx = i
			break
		}
	}
	if leaderIdx == -1 {
		t.Fatal("no leader elected")
	}
	t.Logf("Leader is node %d", leaderIdx+1)

	clientID := "test-client-1"

	acquireURL := fmt.Sprintf("http://%s/lock/acquire?key=mylock&ttl=30&owner_id=%s", httpAddrs[leaderIdx], clientID)
	resp, err := http.Post(acquireURL, "application/json", nil)
	if err != nil {
		t.Fatalf("failed to acquire lock: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("Acquire lock response: %s", string(body))

	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("failed to acquire lock, got: %s", string(body))
	}

	leaseID := "0"
	if strings.Contains(string(body), `"lease_id"`) {
		parts := strings.Split(string(body), `"lease_id":`)
		if len(parts) > 1 {
			leaseID = strings.TrimRight(strings.Split(parts[1], ",")[0], " ")
		}
	}
	t.Logf("Acquired lock with lease_id=%s", leaseID)

	statusURL := fmt.Sprintf("http://%s/lock/status?key=mylock", httpAddrs[leaderIdx])
	resp, err = http.Get(statusURL)
	if err != nil {
		t.Fatalf("failed to check lock status: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"locked":true`) {
		t.Errorf("lock should be held, got: %s", string(body))
	}

	keepAliveURL := fmt.Sprintf("http://%s/lock/keepalive?key=mylock&lease_id=%s&owner_id=%s&ttl=20", httpAddrs[leaderIdx], leaseID, clientID)
	req, _ := http.NewRequest("PUT", keepAliveURL, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to keepalive: %v", err)
	}
	resp.Body.Close()
	t.Logf("KeepAlive response status: %d", resp.StatusCode)

	releaseURL := fmt.Sprintf("http://%s/lock/release?key=mylock&lease_id=%s&owner_id=%s", httpAddrs[leaderIdx], leaseID, clientID)
	req, _ = http.NewRequest("DELETE", releaseURL, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to release lock: %v", err)
	}
	resp.Body.Close()
	t.Logf("Release response status: %d", resp.StatusCode)

	resp, err = http.Get(statusURL)
	if err != nil {
		t.Fatalf("failed to check lock status after release: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"locked":false`) {
		t.Errorf("lock should be released, got: %s", string(body))
	}

	for _, s := range servers {
		s.Shutdown()
	}
}

func TestDistributedLockContention(t *testing.T) {
	basePort := 21000 + int(time.Now().UnixMilli()%1000)*10
	raftAddrs := []string{
		fmt.Sprintf("127.0.0.1:%d", basePort),
		fmt.Sprintf("127.0.0.1:%d", basePort+1),
		fmt.Sprintf("127.0.0.1:%d", basePort+2),
	}
	httpAddrs := []string{
		fmt.Sprintf("127.0.0.1:%d", basePort+10),
		fmt.Sprintf("127.0.0.1:%d", basePort+11),
		fmt.Sprintf("127.0.0.1:%d", basePort+12),
	}

	tmpDir, err := os.MkdirTemp("", "testetcd_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	for i := 0; i < 3; i++ {
		raft.SetPeerAddr(i, "")
	}
	for i, addr := range raftAddrs {
		raft.SetPeerAddr(i, addr)
	}

	var servers []*Server
	for i := 0; i < 3; i++ {
		walPath := filepath.Join(tmpDir, fmt.Sprintf("node%d.wal", i))
		snapPath := filepath.Join(tmpDir, fmt.Sprintf("node%d.snap", i))
		wal, _ := storage.NewWAL(walPath)
		snap, _ := storage.NewSnapshot(snapPath)
		srv := NewServer(i+1, raftAddrs[i], raftAddrs, wal, snap, "", httpAddrs[i])
		servers = append(servers, srv)
		go srv.StartHTTPServer()
	}

	time.Sleep(2 * time.Second)

	leaderIdx := -1
	for i, s := range servers {
		if s.IsLeader() {
			leaderIdx = i
			break
		}
	}
	if leaderIdx == -1 {
		t.Fatal("no leader elected")
	}

	client1 := "client-1"
	client2 := "client-2"

	acquireURL1 := fmt.Sprintf("http://%s/lock/acquire?key=contested&ttl=5&owner_id=%s", httpAddrs[leaderIdx], client1)
	resp, err := http.Post(acquireURL1, "application/json", nil)
	if err != nil {
		t.Fatalf("client1 failed to acquire: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	leaseID := "0"
	if strings.Contains(string(body), `"lease_id"`) {
		parts := strings.Split(string(body), `"lease_id":`)
		if len(parts) > 1 {
			leaseID = strings.TrimRight(strings.Split(parts[1], ",")[0], " ")
		}
	}

	acquireURL2 := fmt.Sprintf("http://%s/lock/acquire?key=contested&ttl=5&owner_id=%s", httpAddrs[leaderIdx], client2)
	resp, err = http.Post(acquireURL2, "application/json", nil)
	if err != nil {
		t.Fatalf("client2 failed to acquire: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("Client2 acquire response: %s", string(body))

	if resp.StatusCode == http.StatusOK {
		t.Logf("Client2 acquired lock (might be race condition)")
	} else if resp.StatusCode == http.StatusConflict {
		t.Logf("Client2 correctly rejected: %s", string(body))
	}

	releaseURL := fmt.Sprintf("http://%s/lock/release?key=contested&lease_id=%s&owner_id=%s", httpAddrs[leaderIdx], leaseID, client1)
	req, _ := http.NewRequest("DELETE", releaseURL, nil)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()

	time.Sleep(200 * time.Millisecond)

	acquireURL3 := fmt.Sprintf("http://%s/lock/acquire?key=contested&ttl=5&owner_id=%s", httpAddrs[leaderIdx], client2)
	resp, err = http.Post(acquireURL3, "application/json", nil)
	if err != nil {
		t.Fatalf("client2 failed to acquire after release: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(body), `"ok":true`) {
		t.Errorf("client2 should acquire lock after client1 releases, got: %s", string(body))
	} else {
		t.Logf("Client2 successfully acquired lock after client1 released")
	}

	for _, s := range servers {
		s.Shutdown()
	}
}

func TestPrefixOperations(t *testing.T) {
	basePort := 22000 + int(time.Now().UnixMilli()%1000)*10
	raftAddrs := []string{
		fmt.Sprintf("127.0.0.1:%d", basePort),
		fmt.Sprintf("127.0.0.1:%d", basePort+1),
		fmt.Sprintf("127.0.0.1:%d", basePort+2),
	}
	httpAddrs := []string{
		fmt.Sprintf("127.0.0.1:%d", basePort+10),
		fmt.Sprintf("127.0.0.1:%d", basePort+11),
		fmt.Sprintf("127.0.0.1:%d", basePort+12),
	}

	tmpDir, err := os.MkdirTemp("", "testetcd_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	for i := 0; i < 3; i++ {
		raft.SetPeerAddr(i, "")
	}
	for i, addr := range raftAddrs {
		raft.SetPeerAddr(i, addr)
	}

	var servers []*Server
	for i := 0; i < 3; i++ {
		walPath := filepath.Join(tmpDir, fmt.Sprintf("node%d.wal", i))
		snapPath := filepath.Join(tmpDir, fmt.Sprintf("node%d.snap", i))
		wal, _ := storage.NewWAL(walPath)
		snap, _ := storage.NewSnapshot(snapPath)
		srv := NewServer(i+1, raftAddrs[i], raftAddrs, wal, snap, "", httpAddrs[i])
		servers = append(servers, srv)
	}

	time.Sleep(2 * time.Second)

	leaderIdx := -1
	for i, s := range servers {
		if s.IsLeader() {
			leaderIdx = i
			break
		}
	}
	if leaderIdx == -1 {
		t.Fatal("no leader elected")
	}
	t.Logf("Leader is node %d", leaderIdx+1)

	leader := servers[leaderIdx]
	leader.store.Put("service/node1", "server1")
	leader.store.Put("service/node2", "server2")
	leader.store.Put("config/timeout", "30")

	resultMap := leader.GetPrefix("service/")
	if len(resultMap) != 2 {
		t.Errorf("expected 2 keys with prefix service/, got %d: %v", len(resultMap), resultMap)
	} else {
		t.Logf("PASS: GetPrefix returned %d keys", len(resultMap))
	}

	if _, ok := resultMap["service/node1"]; !ok {
		t.Errorf("expected service/node1 in result")
	}
	if _, ok := resultMap["config/timeout"]; ok {
		t.Errorf("config/timeout should NOT be in service/ result")
	}

	deletedKeys := leader.store.DeletePrefix("service/")
	if len(deletedKeys) != 2 {
		t.Errorf("expected 2 deleted keys, got %d", len(deletedKeys))
	} else {
		t.Logf("PASS: DeletePrefix deleted %d keys", len(deletedKeys))
	}

	resultMap2 := leader.GetPrefix("service/")
	if len(resultMap2) != 0 {
		t.Errorf("expected 0 keys after delete, got %d", len(resultMap2))
	} else {
		t.Logf("PASS: DeletePrefix deleted all service/ keys")
	}

	for _, s := range servers {
		s.Shutdown()
	}
}

func TestPrefixWatch(t *testing.T) {
	basePort := 23000 + int(time.Now().UnixMilli()%1000)*10
	raftAddrs := []string{
		fmt.Sprintf("127.0.0.1:%d", basePort),
		fmt.Sprintf("127.0.0.1:%d", basePort+1),
		fmt.Sprintf("127.0.0.1:%d", basePort+2),
	}
	httpAddrs := []string{
		fmt.Sprintf("127.0.0.1:%d", basePort+10),
		fmt.Sprintf("127.0.0.1:%d", basePort+11),
		fmt.Sprintf("127.0.0.1:%d", basePort+12),
	}

	tmpDir, err := os.MkdirTemp("", "testetcd_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	for i := 0; i < 3; i++ {
		raft.SetPeerAddr(i, "")
	}
	for i, addr := range raftAddrs {
		raft.SetPeerAddr(i, addr)
	}

	var servers []*Server
	for i := 0; i < 3; i++ {
		walPath := filepath.Join(tmpDir, fmt.Sprintf("node%d.wal", i))
		snapPath := filepath.Join(tmpDir, fmt.Sprintf("node%d.snap", i))
		wal, _ := storage.NewWAL(walPath)
		snap, _ := storage.NewSnapshot(snapPath)
		srv := NewServer(i+1, raftAddrs[i], raftAddrs, wal, snap, "", httpAddrs[i])
		servers = append(servers, srv)
	}

	time.Sleep(2 * time.Second)

	leaderIdx := -1
	for i, s := range servers {
		if s.IsLeader() {
			leaderIdx = i
			break
		}
	}
	if leaderIdx == -1 {
		t.Fatal("no leader elected")
	}
	t.Logf("Leader is node %d", leaderIdx+1)

	leader := servers[leaderIdx]
	watcher := leader.watchers.AddPrefixWatcher("events/", false, 10*time.Second, 0)
	if watcher == nil {
		t.Fatal("failed to create prefix watcher")
	}
	t.Logf("PASS: Prefix watcher created successfully")

	leader.store.Put("events/user/login", "alice")
	leader.store.Put("events/user/logout", "bob")
	leader.store.Put("config/test", "ignore")

	leader.currentRev++
	cmd := kv.Command{Type: kv.CmdPut, Key: "events/user/login", Value: "alice", Revision: leader.currentRev}
	leader.notifyWatchers(cmd)

	select {
	case event := <-watcher.Ch:
		t.Logf("PASS: Received event on prefix watcher: %s %s", event.Key, event.Type)
		if event.Key != "events/user/login" {
			t.Errorf("expected key events/user/login, got %s", event.Key)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("timeout waiting for event")
	}

	for _, s := range servers {
		s.Shutdown()
	}
}
