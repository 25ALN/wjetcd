package config

// 单个节点配置
type NodeConfig struct {
	ID       int
	RaftAddr string
	RPCAddr  string
	HTTPAddr string
}

// 集群配置
type ClusterConfig struct {
	Nodes []NodeConfig
}

// 预定义配置
var TestCluster = ClusterConfig{
	Nodes: []NodeConfig{
		{ID: 0, RaftAddr: "127.0.0.1:5000", RPCAddr: "127.0.0.1:6000", HTTPAddr: "127.0.0.1:8000"},
		{ID: 1, RaftAddr: "127.0.0.1:5001", RPCAddr: "127.0.0.1:6001", HTTPAddr: "127.0.0.1:8001"},
		{ID: 2, RaftAddr: "127.0.0.1:5002", RPCAddr: "127.0.0.1:6002", HTTPAddr: "127.0.0.1:8002"},
	},
}

// GetNodeConfig: 获取指定ID的节点配置
func (c *ClusterConfig) GetNodeConfig(id int) *NodeConfig {
	for _, node := range c.Nodes {
		if node.ID == id {
			return &node
		}
	}
	return nil
}

// GetRaftAddrs: 获取所有Raft地址列表
func (c *ClusterConfig) GetRaftAddrs() []string {
	addrs := make([]string, len(c.Nodes))
	for i, node := range c.Nodes {
		addrs[i] = node.RaftAddr
	}
	return addrs
}
