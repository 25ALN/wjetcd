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
)

type Command struct {
	Type    CommandType
	Key     string
	Value   string
	LeaseID int64
	TTL    int64
}
