package main

import (
	"flag"
	"fmt"
	"log"
	"strings"

	"testetcd/raft"
	"testetcd/server"
	"testetcd/storage"
)

func main() {

	id := flag.Int("id", 1, "node id")
	raftAddr := flag.String("raft", "127.0.0.1:8001", "raft address")
	httpAddr := flag.String("http", "127.0.0.1:9001", "http address")
	rpcAddr := flag.String("rpc", "127.0.0.1:7001", "rpc address")

	peers := flag.String("peers", "127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003", "peer raft addresses")

	flag.Parse()

	// 解析 peers
	peerList := strings.Split(*peers, ",")

	log.Printf("Node %d starting...", *id)
	log.Printf("RaftAddr: %s", *raftAddr)
	log.Printf("Peers: %v", peerList)
	// 初始化存储

	wal, err := storage.NewWAL(fmt.Sprintf("node%d.wal", *id))
	if err != nil {
		log.Fatalf("Failed to create WAL: %v", err)
	}
	defer wal.Close()

	snapshot, err := storage.NewSnapshot(fmt.Sprintf("node%d.snap", *id))
	if err != nil {
		log.Fatalf("Failed to create snapshot: %v", err)
	}
	defer snapshot.Close()
	// 注册 peers

	for i, addr := range peerList {
		raft.SetPeerAddr(i, addr)
	}
	// 创建 serve
	srvr := server.NewServer(
		*id,
		*raftAddr,
		peerList,
		wal,
		snapshot,
		*rpcAddr,
		*httpAddr,
	)
	// 启动服务

	go srvr.StartRPCServer()

	if err := srvr.StartHTTPServer(); err != nil {
		log.Fatalf("HTTP server error: %v", err)
	}

	log.Printf("Node %d is running...", *id)

	select {}
}
