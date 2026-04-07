package raftapi

// The Raft interface
type Raft interface {
	Start(command interface{}) (int, int, bool)
	GetState() (int, bool)
	Snapshot(index int, snapshot []byte)
	PersistBytes() int
	Kill()
}

type ApplyMsg struct {
	CommandValid bool
	Command      interface{}
	CommandIndex int

	SnapshotValid bool
	Snapshot      []byte
	SnapshotTerm  int
	SnapshotIndex int
}
