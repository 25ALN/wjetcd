package main

import (
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Result struct {
	Latency time.Duration
	OK      bool
	Err     string
	Body    string
}

type Stats struct {
	Total     int
	Success   int
	Failed    int
	Latencies []time.Duration
}

func (s *Stats) Avg() time.Duration {
	if len(s.Latencies) == 0 {
		return 0
	}
	var sum time.Duration
	for _, l := range s.Latencies {
		sum += l
	}
	return sum / time.Duration(len(s.Latencies))
}

func (s *Stats) Pctile(p float64) time.Duration {
	if len(s.Latencies) == 0 {
		return 0
	}
	idx := int(float64(len(s.Latencies)-1) * p)
	return s.Latencies[idx]
}

func findLeader(addrs []string) string {
	for _, addr := range addrs {
		url := fmt.Sprintf("http://%s/health", addr)
		resp, err := http.Get(url)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(body), `"status":"leader"`) {
			return addr
		}
	}
	return ""
}

func httpPut(addr, key, value string) Result {
	start := time.Now()
	url := fmt.Sprintf("http://%s/put?key=%s&value=%s", addr, key, value)
	resp, err := http.Post(url, "application/json", nil)
	lat := time.Since(start)
	if err != nil {
		return Result{Latency: lat, OK: false, Err: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	bs := string(body)
	ok := resp.StatusCode == 200 && strings.Contains(bs, `"ok":true`)
	errMsg := ""
	if !ok {
		errMsg = strings.TrimSpace(bs)
	}
	return Result{Latency: lat, OK: ok, Err: errMsg, Body: bs}
}

func httpGet(addr, key string) Result {
	start := time.Now()
	url := fmt.Sprintf("http://%s/get?key=%s", addr, key)
	resp, err := http.Get(url)
	lat := time.Since(start)
	if err != nil {
		return Result{Latency: lat, OK: false, Err: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	ok := resp.StatusCode == 200
	errMsg := ""
	if !ok {
		errMsg = strings.TrimSpace(string(body))
	}
	return Result{Latency: lat, OK: ok, Err: errMsg, Body: string(body)}
}

func httpDelete(addr, key string) Result {
	start := time.Now()
	url := fmt.Sprintf("http://%s/delete?key=%s", addr, key)
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	lat := time.Since(start)
	if err != nil {
		return Result{Latency: lat, OK: false, Err: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	bs := string(body)
	ok := resp.StatusCode == 200 && strings.Contains(bs, `"ok":true`)
	errMsg := ""
	if !ok {
		errMsg = strings.TrimSpace(bs)
	}
	return Result{Latency: lat, OK: ok, Err: errMsg, Body: bs}
}

func httpLeaseGrant(addr string, ttl int) Result {
	start := time.Now()
	url := fmt.Sprintf("http://%s/lease/grant?ttl=%d", addr, ttl)
	resp, err := http.Post(url, "application/json", nil)
	lat := time.Since(start)
	if err != nil {
		return Result{Latency: lat, OK: false, Err: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	bs := string(body)
	ok := resp.StatusCode == 200 && strings.Contains(bs, `"ok":true`)
	errMsg := ""
	if !ok {
		errMsg = strings.TrimSpace(bs)
	}
	return Result{Latency: lat, OK: ok, Err: errMsg, Body: bs}
}

func httpLeaseRevoke(addr string, leaseID string) Result {
	start := time.Now()
	url := fmt.Sprintf("http://%s/lease/revoke?lease_id=%s", addr, leaseID)
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	resp, err := http.DefaultClient.Do(req)
	lat := time.Since(start)
	if err != nil {
		return Result{Latency: lat, OK: false, Err: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	bs := string(body)
	ok := resp.StatusCode == 200 && strings.Contains(bs, `"ok":true`)
	errMsg := ""
	if !ok {
		errMsg = strings.TrimSpace(bs)
	}
	return Result{Latency: lat, OK: ok, Err: errMsg, Body: bs}
}

func extractLeaseID(body string) string {
	idx := strings.Index(body, `"lease_id":`)
	if idx < 0 {
		return ""
	}
	start := idx + 11
	end := start
	for end < len(body) && body[end] != '}' && body[end] != ',' {
		end++
	}
	return body[start:end]
}

func printStats(name string, s *Stats) {
	if len(s.Latencies) > 0 {
		sort.Slice(s.Latencies, func(i, j int) bool { return s.Latencies[i] < s.Latencies[j] })
	}
	fmt.Printf("\n--- %s ---\n", name)
	fmt.Printf("  Total:   %d\n", s.Total)
	fmt.Printf("  Success: %d\n", s.Success)
	fmt.Printf("  Failed:  %d\n", s.Failed)
	if s.Total > 0 {
		fmt.Printf("  Rate:    %.2f%%\n", float64(s.Success)/float64(s.Total)*100)
	}
	if len(s.Latencies) > 0 {
		fmt.Printf("  Avg:     %v\n", s.Avg())
		fmt.Printf("  P50:     %v\n", s.Pctile(0.5))
		fmt.Printf("  P95:     %v\n", s.Pctile(0.95))
		fmt.Printf("  P99:     %v\n", s.Pctile(0.99))
		fmt.Printf("  Min:     %v\n", s.Latencies[0])
		fmt.Printf("  Max:     %v\n", s.Latencies[len(s.Latencies)-1])
	}
}

func runTest(concurrency, total int, duration time.Duration, fn func() Result) *Stats {
	var mu sync.Mutex
	var wg sync.WaitGroup
	var ops atomic.Int64
	var success atomic.Int64
	var failed atomic.Int64
	var latencies []time.Duration

	deadline := time.Now()
	if duration > 0 {
		deadline = deadline.Add(duration)
	}

	limiter := make(chan struct{}, concurrency)
	done := make(chan struct{})

	go func() {
		if duration > 0 {
			for time.Now().Before(deadline) {
				select {
				case limiter <- struct{}{}:
					wg.Add(1)
					ops.Add(1)
					go func() {
						defer wg.Done()
						defer func() { <-limiter }()
						res := fn()
						mu.Lock()
						latencies = append(latencies, res.Latency)
						mu.Unlock()
						if res.OK {
							success.Add(1)
						} else {
							failed.Add(1)
						}
					}()
				default:
					time.Sleep(5 * time.Millisecond)
				}
			}
		} else {
			for i := 0; i < total; i++ {
				select {
				case limiter <- struct{}{}:
					wg.Add(1)
					ops.Add(1)
					go func() {
						defer wg.Done()
						defer func() { <-limiter }()
						res := fn()
						mu.Lock()
						latencies = append(latencies, res.Latency)
						mu.Unlock()
						if res.OK {
							success.Add(1)
						} else {
							failed.Add(1)
						}
					}()
				default:
					time.Sleep(5 * time.Millisecond)
				}
			}
		}
		wg.Wait()
		close(done)
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return &Stats{
				Total:     int(success.Load()) + int(failed.Load()),
				Success:   int(success.Load()),
				Failed:    int(failed.Load()),
				Latencies: latencies,
			}
		case <-ticker.C:
			s := success.Load()
			f := failed.Load()
			fmt.Printf("  [progress] %d ok / %d fail / %d ops\n", s, f, ops.Load())
		}
	}
}

func main() {
	addrsStr := flag.String("addrs", "127.0.0.1:9001,127.0.0.1:9002,127.0.0.1:9003", "comma-separated server HTTP addresses")
	concurrency := flag.Int("c", 20, "concurrent goroutines")
	total := flag.Int("n", 500, "total requests per operation")
	valueSize := flag.Int("vsize", 100, "value size in bytes")
	duration := flag.Duration("d", 0, "duration mode (e.g. 30s); if set, -n is ignored")
	flag.Parse()

	addrs := strings.Split(*addrsStr, ",")
	leader := findLeader(addrs)
	if leader == "" {
		fmt.Println("Error: no leader found. Is the cluster running?")
		return
	}
	fmt.Printf("Leader:    %s\n", leader)
	fmt.Printf("Workers:   %d\n", *concurrency)
	if *duration > 0 {
		fmt.Printf("Duration:  %v\n", *duration)
	} else {
		fmt.Printf("Requests:  %d per test\n", *total)
	}
	fmt.Printf("ValueSize: %d bytes\n", *valueSize)
	fmt.Println(strings.Repeat("=", 50))

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	valueBytes := make([]byte, *valueSize)
	for i := range valueBytes {
		valueBytes[i] = byte('a' + rng.Intn(26))
	}
	fixedValue := string(valueBytes)

	totalWallStart := time.Now()

	// Phase 1: Write (Put)
	fmt.Println("\n[Phase 1] Write (Put)...")
	writeStats := runTest(*concurrency, *total, *duration, func() Result {
		key := fmt.Sprintf("stress_%d", rng.Int63())
		return httpPut(leader, key, fixedValue)
	})
	printStats("PUT", writeStats)

	// Phase 2: Read (Get) - pre-populate keys
	fmt.Println("\n[Phase 2] Read (Get)...")
	preKeys := make([]string, 1000)
	for i := range preKeys {
		preKeys[i] = fmt.Sprintf("stress_%d", i)
		_ = httpPut(leader, preKeys[i], fixedValue)
	}
	readStats := runTest(*concurrency, *total, *duration, func() Result {
		key := preKeys[rng.Intn(len(preKeys))]
		return httpGet(leader, key)
	})
	printStats("GET", readStats)

	// Phase 3: Delete
	fmt.Println("\n[Phase 3] Delete...")
	delKeys := make([]string, *total)
	for i := range delKeys {
		delKeys[i] = fmt.Sprintf("del_%d", i)
		_ = httpPut(leader, delKeys[i], fixedValue)
	}
	var delIdx atomic.Int64
	delStats := runTest(*concurrency, *total, *duration, func() Result {
		idx := int(delIdx.Add(1)-1) % len(delKeys)
		return httpDelete(leader, delKeys[idx])
	})
	printStats("DELETE", delStats)

	// Phase 4: Mixed (70% Get, 20% Put, 10% Delete)
	fmt.Println("\n[Phase 4] Mixed (70/20/10)...")
	for i := 0; i < 200; i++ {
		_ = httpPut(leader, fmt.Sprintf("mix_%d", i), fixedValue)
	}
	var mixIdx atomic.Int64
	mixStats := runTest(*concurrency, *total, *duration, func() Result {
		r := rng.Intn(100)
		idx := int(mixIdx.Add(1))
		if r < 70 {
			return httpGet(leader, fmt.Sprintf("mix_%d", idx%200))
		} else if r < 90 {
			return httpPut(leader, fmt.Sprintf("mix_%d", idx%300), fixedValue)
		}
		return httpDelete(leader, fmt.Sprintf("mix_%d", idx%200))
	})
	printStats("MIXED", mixStats)

	// Phase 5: Lease Grant/Revoke
	fmt.Println("\n[Phase 5] Lease Grant + Revoke...")
	leaseStats := runTest(*concurrency, *total, *duration, func() Result {
		res := httpLeaseGrant(leader, 30)
		if !res.OK {
			return res
		}
		leaseID := extractLeaseID(res.Body)
		if leaseID == "" {
			return Result{Latency: res.Latency, OK: false, Err: "no lease_id in response", Body: res.Body}
		}
		res2 := httpLeaseRevoke(leader, leaseID)
		res2.Latency += res.Latency
		return res2
	})
	printStats("LEASE (grant+revoke)", leaseStats)

	totalElapsed := time.Since(totalWallStart)
	fmt.Printf("\n%s\n", strings.Repeat("=", 50))
	fmt.Printf("Total wall time: %v\n", totalElapsed)

	allSuccess := writeStats.Success + readStats.Success + delStats.Success + mixStats.Success + leaseStats.Success
	allTotal := writeStats.Total + readStats.Total + delStats.Total + mixStats.Total + leaseStats.Total
	if totalElapsed.Seconds() > 0 {
		fmt.Printf("Overall throughput: %.0f req/s\n", float64(allSuccess)/totalElapsed.Seconds())
	}
	fmt.Printf("Overall success: %d / %d (%.1f%%)\n", allSuccess, allTotal, float64(allSuccess)/float64(allTotal)*100)
}
