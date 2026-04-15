package raft

import (
	"fmt"
)

type Entry struct {
	Term    int
	Command interface{}
}
type Log struct {
	Entries       []Entry
	FirstLogIndex int
	LastLogIndex  int
}

func NewLog() *Log {
	return &Log{
		Entries:       make([]Entry, 0),
		FirstLogIndex: 0,
		//LastLogIndex:  0,
		LastLogIndex: -1,
	}
}
func (log *Log) getRealIndex(index int) int {
	return index - log.FirstLogIndex
}
func (log *Log) getOneEntry(index int) *Entry {

	realIdx := log.getRealIndex(index)
	if realIdx < 0 || realIdx >= len(log.Entries) {
		return nil
	}
	return &log.Entries[realIdx]
}

func (log *Log) appendL(newEntries ...Entry) {
	log.Entries = append(log.Entries[:log.getRealIndex(log.LastLogIndex)+1], newEntries...)
	log.LastLogIndex += len(newEntries)

}

// 这是按照相对FirstLogIndex的偏移量来取日志的
// func (log *Log) getAppendEntries(start int) []Entry {
// 	ret := append([]Entry{}, log.Entries[log.getRealIndex(start):log.getRealIndex(log.LastLogIndex)+1]...)
// 	return ret
// }

func (log *Log) getAppendEntries(start int) []Entry {
	if log.FirstLogIndex > log.LastLogIndex {
		return nil
	}
	if start > log.LastLogIndex {
		return []Entry{}
	}
	startIdx := log.getRealIndex(start)
	endIdx := log.getRealIndex(log.LastLogIndex)

	if startIdx < 0 {
		startIdx = 0
	}
	if endIdx >= len(log.Entries) {
		endIdx = len(log.Entries) - 1
	}
	if startIdx > endIdx {
		return []Entry{}
	}

	return append([]Entry{}, log.Entries[startIdx:endIdx+1]...)
}
func (log *Log) String() string {
	if log.empty() {
		return "logempty"
	}
	return fmt.Sprintf("%v", log.getAppendEntries(log.FirstLogIndex))
}
func (log *Log) empty() bool {
	return log.FirstLogIndex > log.LastLogIndex
}
func (rf *Raft) GetLogEntries() []Entry {
	return rf.log.Entries
}
func (rf *Raft) getEntryTerm(index int) int {
	if index == rf.log.FirstLogIndex-1 {
		return rf.snapshotLastIncludeTerm
	}
	if index < rf.log.FirstLogIndex || index > rf.log.LastLogIndex {
		return -1
	}
	return rf.log.getOneEntry(index).Term
}
