package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func main() {
	//httpAddr := flag.String("addr", "127.0.0.1:9001", "HTTP server address")
	httpAddr := flag.String("addrs", "127.0.0.1:9001,127.0.0.1:9002,127.0.0.1:9003", "server addresses")
	op := flag.String("op", "", "operation: put or get")
	key := flag.String("key", "", "key")
	value := flag.String("value", "", "value (for put)")

	flag.Parse()

	if *op == "" {
		fmt.Println("Usage: client no message")
		return
	}

	baseURL := fmt.Sprintf("http://%s", *httpAddr)
	addrlist := strings.Split(*httpAddr, ",")
	if *op == "put" {
		if *key == "" || *value == "" {
			fmt.Println("put operation requires -key and -value")
			return
		}
		//err := httpPut(baseURL, *key, *value)
		err := httpPut(addrlist, *key, *value)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}
		fmt.Println("Put succeeded")

	} else if *op == "get" {
		if *key == "" {
			fmt.Println("get operation requires -key")
			return
		}
		val, err := httpGet(addrlist, *key)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}
		if len(val) == 0 {
			fmt.Println("This key is not exist or it's empty")
		} else {
			fmt.Printf("Value: %s\n", val)
		}

	} else if *op == "health" {
		status, err := httpHealth(baseURL)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}
		fmt.Printf("Status: %s\n", status)

	} else {
		fmt.Println("Unknown operation:", *op)
	}
}

func httpPut(addrs []string, key, value string) error {
	for _, addr := range addrs {
		url := fmt.Sprintf("http://%s/put?key=%s&value=%s", addr, key, value)

		resp, err := http.Post(url, "application/json", nil)
		if err != nil {
			continue
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)

		if resp.StatusCode == http.StatusOK {
			fmt.Println("Write success on", addr)
			return nil
		}

		if strings.Contains(string(body), "not leader") {
			fmt.Println("Not leader:", addr)
			continue
		}

		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	return fmt.Errorf("all nodes failed (no leader?)")
}

func httpGet(addrs []string, key string) (string, error) {
	for _, addr := range addrs {
		url := fmt.Sprintf("http://%s/get?key=%s", addr, key)
		resp, err := http.Get(url)
		if err != nil {
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		content := string(body)
		idx := strings.Index(content, "\"value\":\"")
		if idx >= 0 {
			start := idx + 9
			nextQuote := strings.Index(content[start:], "\"")
			if nextQuote >= 0 {
				val := content[start : start+nextQuote]
				if len(val) > 0 {
					return val, nil
				}
			}
		}
		continue
	}
	return "", fmt.Errorf("key not found")
}

func httpHealth(baseURL string) (string, error) {
	url := fmt.Sprintf("%s/health", baseURL)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	content := string(body)

	// 简单的JSON解析，提取status字段
	idx := strings.Index(content, "\"status\":\"")
	if idx >= 0 {
		start := idx + 10
		end := strings.Index(content[start:], "\"")
		if end >= 0 {
			return content[start : start+end], nil
		}
	}
	return "", nil
}
