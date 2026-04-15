package raft

import (
	"log"
	"math/rand"
	"time"
)

// let the base election timeout be T.
// the election timeout is in the range [T, 2T).
const baseElectionTimeout = 1000
const None = -1

func (rf *Raft) StartElection() {
	rf.Mu.Lock()
	rf.becomeCandidate()
	term := rf.CurrentTerm
	args := RequestVoteArgs{
		Term:         rf.CurrentTerm,
		CandidateId:  rf.me,
		LastLogIndex: rf.log.LastLogIndex,
		LastLogTerm:  rf.getLastEntryTerm(),
	}
	rf.persist()
	rf.electionVotes = 1
	rf.electionDone = false
	totalPeers := len(rf.peers)
	log.Printf("[%d] start election at term %d, voted for self", rf.me, term)
	rf.Mu.Unlock()

	for i, addr := range rf.peers {
		if rf.me == i {
			continue
		}
		go func(serverId int, serverAddr string, args RequestVoteArgs, term int) {
			var reply RequestVoteReply
			ok := rf.sendRequestVote(serverAddr, &args, &reply)
			rf.Mu.Lock()
			defer rf.Mu.Unlock()
			if !ok {
				log.Printf("[%d] sendRequestVote to %d failed", rf.me, serverId)
				return
			}
			if rf.CurrentTerm != term {
				log.Printf("[%d] term changed, abandon election", rf.me)
				return
			}
			if rf.State != Candidate {
				log.Printf("[%d] state is not candidate, abandon election,term is %d", rf.me, rf.CurrentTerm)
				return
			}
			if rf.electionDone {
				log.Printf("[%d] election already done, abandon election", rf.me)
				return
			}
			if reply.Term > rf.CurrentTerm {
				log.Printf("[%d] found higher term %d from %d, step down", rf.me, reply.Term, serverId)
				rf.CurrentTerm = reply.Term
				rf.State = Follower
				rf.votedFor = None
				rf.persist()
				rf.electionDone = true
				return
			}
			if reply.Term < rf.CurrentTerm {
				log.Printf("[%d] reply term %d < current term %d, ignore", rf.me, reply.Term, rf.CurrentTerm)
				return
			}
			if reply.VoteGranted {
				rf.electionVotes++
				log.Printf("[%d] got vote from %d, now %d/%d", rf.me, serverId, rf.electionVotes, totalPeers)
				if !rf.electionDone && rf.electionVotes > totalPeers/2 {
					log.Printf("[%d] become leader at term %d", rf.me, term)
					rf.becomeLeader()
					rf.electionDone = true
				}
			} else {
				log.Printf("[%d] vote denied by %d", rf.me, serverId)
			}
		}(i, addr, args, term)
	}
}

func (rf *Raft) pastElectionTimeout() bool {
	rf.Mu.Lock()
	defer rf.Mu.Unlock()
	return time.Since(rf.lastElection) > rf.electionTimeout
}

func (rf *Raft) resetElectionTimer() {
	electionTimeout := baseElectionTimeout + (rand.Int63() % baseElectionTimeout)
	rf.electionTimeout = time.Duration(electionTimeout) * time.Millisecond
	rf.lastElection = time.Now()
	//log.Printf("%d has refreshed the electionTimeout at term %d to a random value %d...\n", rf.me, rf.CurrentTerm, rf.electionTimeout/1000000)
}

func (rf *Raft) becomeCandidate() {
	rf.resetElectionTimer()
	log.Printf("%d preparing to start election at term %d", rf.State, rf.CurrentTerm)
	rf.State = Candidate
	rf.CurrentTerm++
	rf.votedFor = rf.me
}

func (rf *Raft) becomeLeader() {

	rf.State = Leader
	log.Printf("[%d] is elected as leader at term %d", rf.me, rf.CurrentTerm)
	// 初始化peerTrackers，nextIndex为LastLogIndex+1，matchIndex为0
	rf.resetTrackedIndex()
	log.Printf("%v :becomes leader and reset TrackedIndex, nextIndex=%d for all peers\n", rf.SayMeL(), rf.log.LastLogIndex+1)
	rf.resetHeartbeatTimer()
	rf.resetElectionTimer()
	// 立即广播一次心跳，防止集群状态不一致
	go rf.StartAppendEntries(true)
}

// 定义一个心跳兼日志同步处理器，这个方法是Candidate和Follower节点的处理
func (rf *Raft) HandleHeartbeatRPC(args *RequestAppendEntriesArgs, reply *RequestAppendEntriesReply) {
	rf.Mu.Lock() // 加接收心跳方的锁
	defer rf.Mu.Unlock()
	reply.FollowerTerm = rf.CurrentTerm
	reply.Success = true
	// 旧任期的leader抛弃掉
	if args.LeaderTerm < rf.CurrentTerm {
		reply.Success = false
		return
	}
	rf.resetElectionTimer()
	// 需要转变自己的身份为Follower

	rf.State = Follower
	log.Printf("received heartbeat from leader %d at term %d, reset election timer\n", args.LeaderId, args.LeaderTerm)
	// 承认来者是个合法的新leader，则任期一定大于自己，此时需要设置votedFor为-1以及
	if args.LeaderTerm > rf.CurrentTerm {
		rf.votedFor = None
		rf.CurrentTerm = args.LeaderTerm
		reply.FollowerTerm = rf.CurrentTerm
		//rf.persist()
	}
	rf.persist()

	// 重置自身的选举定时器，这样自己就不会重新发出选举需求（因为它在ticker函数中被阻塞住了）
}

// example RequestVoteRPC handler.
func (rf *Raft) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	rf.Mu.Lock()
	defer rf.Mu.Unlock()
	reply.VoteGranted = true // 默认设置响应体为投同意票状态
	reply.Term = rf.CurrentTerm
	//竞选leader的节点任期小于等于自己的任期，则反对票(为什么等于情况也反对票呢？因为candidate节点在发送requestVote rpc之前会将自己的term+1)
	if args.Term < rf.CurrentTerm {
		reply.Term = rf.CurrentTerm
		reply.VoteGranted = false
		rf.persist()
		log.Printf("%v: 投出反对票给节点%d，因为它的任期%d小于我的任期%d", rf.SayMeL(), args.CandidateId, args.Term, rf.CurrentTerm)
		return
	}
	if args.Term > rf.CurrentTerm {
		rf.CurrentTerm = args.Term
		rf.votedFor = None
		reply.Term = rf.CurrentTerm
		rf.persist()
	}
	DPrintf(500, "%v: reply to %v myLastLogterm=%v myLastLogIndex=%v args.LastLogTerm=%v args.LastLogIndex=%v\n",
		rf.SayMeL(), args.CandidateId, rf.getLastEntryTerm(), rf.log.LastLogIndex, args.LastLogTerm, args.LastLogIndex)

	update := false
	// candidate节点发送过来的日志索引以及任期必须大于等于自己的日志索引及任期
	if rf.log.LastLogIndex < rf.log.FirstLogIndex {
		// 日志为空，允许投票
		update = true
	} else {
		update = args.LastLogTerm > rf.getLastEntryTerm()
		update = update || (args.LastLogTerm == rf.getLastEntryTerm() && args.LastLogIndex >= rf.log.LastLogIndex)
	}
	if (rf.votedFor == -1 || rf.votedFor == args.CandidateId) && update {
		// 只更新votedFor和选举定时器，不主动降级为Follower
		rf.votedFor = args.CandidateId
		rf.resetElectionTimer()
		log.Printf("%v: 投出同意票给节点%d", rf.SayMeL(), args.CandidateId)
		rf.persist()

	} else {
		reply.VoteGranted = false
		reply.Term = rf.CurrentTerm
		log.Printf("%v: 投出反对票给节点%d", rf.SayMeL(), args.CandidateId)
	}
	// 只要收到投票请求，无论同意与否都重置选举超时，防止选举风暴
	rf.resetElectionTimer()

}
