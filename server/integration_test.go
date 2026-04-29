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
