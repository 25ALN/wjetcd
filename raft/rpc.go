package raft

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
)

// 启动 Raft 节点 HTTP 服务器
func (rf *Raft) StartHTTPServer(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/raft/append_entries", rf.handleAppendEntries)
	mux.HandleFunc("/raft/request_vote", rf.handleRequestVote)
	mux.HandleFunc("/raft/install_snapshot", rf.handleInstallSnapshot)
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Fatalf("Raft HTTP server failed: %v", err)
		}
	}()
	return nil
}

// 处理 AppendEntries 请求
func (rf *Raft) handleAppendEntries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var args RequestAppendEntriesArgs
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &args)
	var reply RequestAppendEntriesReply
	// 直接调用Raft的AppendEntries RPC实现
	rf.handleAppendEntriesRPC(&args, &reply)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

// 实际AppendEntries RPC实现，参数与Raft RPC一致
func (rf *Raft) handleAppendEntriesRPC(args *RequestAppendEntriesArgs, reply *RequestAppendEntriesReply) {
	rf.Mu.Lock()
	defer rf.Mu.Unlock()
	reply.FollowerTerm = rf.CurrentTerm
	if args.LeaderTerm < rf.CurrentTerm {
		reply.Success = false
		return
	}
	if args.LeaderTerm > rf.CurrentTerm {
		rf.CurrentTerm = args.LeaderTerm
		rf.votedFor = -1
		rf.persist()
		reply.FollowerTerm = rf.CurrentTerm
	}
	rf.State = Follower
	rf.resetElectionTimer()

	// 日志匹配检查
	if args.PrevLogIndex > 0 {
		if rf.log.LastLogIndex < args.PrevLogIndex {
			reply.Success = false
			return
		}
		if rf.log.getOneEntry(args.PrevLogIndex).Term != args.PrevLogTerm {
			reply.Success = false
			return
		}
	}

	// 追加或覆盖日志
	if len(args.Entries) > 0 {
		idx := args.PrevLogIndex
		for _, entry := range args.Entries {
			idx++
			if idx < rf.log.FirstLogIndex {
				continue
			}
			if idx <= rf.log.LastLogIndex {
				// 存在冲突，覆盖
				if rf.log.getOneEntry(idx).Term != entry.Term {
					rf.log.Entries = rf.log.Entries[:rf.log.getRealIndex(idx)]
					rf.log.LastLogIndex = idx - 1
					rf.log.Entries = append(rf.log.Entries, entry)
					rf.log.LastLogIndex++
				}
			} else {
				// 追加新日志
				rf.log.Entries = append(rf.log.Entries, entry)
				rf.log.LastLogIndex++
			}
		}
	}

	// 更新 CommitIndex
	if args.LeaderCommit > rf.CommitIndex {
		rf.CommitIndex = min(args.LeaderCommit, rf.log.LastLogIndex)
		rf.applyCond.Broadcast()
	}

	reply.Success = true
}

// 处理 RequestVote 请求
func (rf *Raft) handleRequestVote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var args RequestVoteArgs
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &args)
	var reply RequestVoteReply
	rf.RequestVote(&args, &reply)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

// 处理 InstallSnapshot 请求
func (rf *Raft) handleInstallSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var args RequestInstallSnapShotArgs
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &args)
	var reply RequestInstallSnapShotReply
	rf.RequestInstallSnapshot(&args, &reply)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

// 发送投票请求（HTTP 客户端）

// 发送日志附加请求（HTTP 客户端）
func (rf *Raft) sendAppendEntries(serverAddr string, args *RequestAppendEntriesArgs, reply *RequestAppendEntriesReply) bool {
	data, _ := json.Marshal(args)
	resp, err := http.Post("http://"+serverAddr+"/raft/append_entries", "application/json", bytes.NewReader(data))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(body, reply)
	return true
}

// 发送快照安装请求（HTTP 客户端）
func (rf *Raft) sendInstallSnapshot(serverAddr string, args *RequestInstallSnapShotArgs, reply *RequestInstallSnapShotReply) bool {
	data, _ := json.Marshal(args)
	resp, err := http.Post("http://"+serverAddr+"/raft/install_snapshot", "application/json", bytes.NewReader(data))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(body, reply)
	return true
}

// 获取对等节点地址
var peerAddrs = make(map[int]string)
var addrMu sync.Mutex

func SetPeerAddr(id int, addr string) {
	addrMu.Lock()
	defer addrMu.Unlock()
	peerAddrs[id] = addr
}

func getPeerAddr(id int) string {
	addrMu.Lock()
	defer addrMu.Unlock()
	if addr, ok := peerAddrs[id]; ok {
		return addr
	}
	return ""
}
