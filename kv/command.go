package kv

type CommandType int

const (
	CmdPut CommandType = iota
	CmdGet
	CmdDelete
	CmdLeaseGrant
	CmdLeaseRevoke
	CmdLeaseAttach
	CmdLeaseKeepAlive
	CmdCAS
	CmdDeletePrefix
	CmdGetPrefix
)

type Command struct {
	Type        CommandType
	Key         string
	Value       string
	LeaseID     int64
	TTL         int64
	Revision    int64
	Tombstone   bool
	ExpectedRev int64
	Prefix      string
}
