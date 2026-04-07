package raft

import (
	"log"
	"time"
)

// if the peer has not acked in this duration, it's considered inactive.
const activeWindowWidth = 2 * baseElectionTimeout * time.Millisecond

type PeerTracker struct {
	nextIndex  int
	matchIndex int

	lastAck time.Time
}

func (rf *Raft) resetTrackedIndex() {

	for i := range rf.peerTrackers {
		rf.peerTrackers[i].nextIndex = rf.log.LastLogIndex + 1
		rf.peerTrackers[i].matchIndex = 0
		log.Printf("实例 %d 更新实例%d的nextIndex为: %d...", rf.me, i, rf.peerTrackers[i].nextIndex)
	}
}
func (rf *Raft) quorumActive() bool {
	activePeers := 1
	for i, tracker := range rf.peerTrackers {
		if i != rf.me && time.Since(tracker.lastAck) <= activeWindowWidth {
			activePeers++
		}
	}
	return 2*activePeers > len(rf.peers)
}
