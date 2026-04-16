package main

import (
	"flag"
	"fmt"
	"log"
	"testetcd/client"
)

func main() {
	fmt.Printf("RPC client start .....")
	rpcAddr := flag.String("rpcaddr", "127.0.0.1:6000", "RPC server address")
	op := flag.String("op", "", "operation: put, get or delete")
	key := flag.String("key", "", "key")
	value := flag.String("value", "", "value (for put)")

	flag.Parse()

	if *op == "" {
		fmt.Println("Usage: rpcclient -op <put|get|delete> -key <key> [-value <value>]")
		return
	}

	c, err := client.NewClient(*rpcAddr)
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer c.Close()

	switch *op {
	case "put":
		if *key == "" || *value == "" {
			fmt.Println("put operation requires -key and -value")
			return
		}
		err := c.Put(*key, *value)
		if err != nil {
			fmt.Printf("Put error: %v\n", err)
			return
		}
		fmt.Println("Put succeeded")

	case "get":
		if *key == "" {
			fmt.Println("get operation requires -key")
			return
		}
		val, err := c.Get(*key)
		if err != nil {
			fmt.Printf("Get error: %v\n", err)
			return
		}
		fmt.Printf("Value: %s\n", val)

	case "delete":
		if *key == "" {
			fmt.Println("delete operation requires -key")
			return
		}
		err := c.Delete(*key)
		if err != nil {
			fmt.Printf("Delete error: %v\n", err)
			return
		}
		fmt.Println("Delete succeeded")

	default:
		fmt.Println("Invalid operation")
	}
}
