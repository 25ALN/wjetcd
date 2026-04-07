package kv

type CommandType int

const (
	CmdPut CommandType = iota
	CmdGet
	CmdDelete
)

type Command struct {
	Type  CommandType
	Key   string
	Value string
}
