package server

import (
	"fmt"
	"log"
	"net/http"
	"testetcd/kv"
	"time"
)

// HTTP Handler for REST API
type HTTPHandler struct {
	server *Server
}

func NewHTTPHandler(s *Server) *HTTPHandler {
	return &HTTPHandler{server: s}
}

// APIResponse for HTTP responses
type APIResponse struct {
	Ok    bool   `json:"ok"`
	Value string `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}

// PutRequest for RPC
type PutRequest struct {
	Key   string
	Value string
}

type PutResponse struct {
	Ok bool
}

type GetRequest struct {
	Key string
}

type GetResponse struct {
	Value string
}

type Handler struct {
	s *Server
}

func NewHandler(s *Server) *Handler {
	return &Handler{s: s}
}

type DeleteRequest struct {
	Key string
}

type DeleteResponse struct {
	Ok bool
}

type WatchRequest struct {
	Key        string
	Persistent bool
	Timeout    int64
	WatcherID  uint64
}

type WatchResponse struct {
	WatcherID uint64
}

type WatchEvent struct {
	Event
}

// RPC: Put
func (h *Handler) Put(req *PutRequest, resp *PutResponse) error {
	cmd := kv.Command{
		Type:  kv.CmdPut,
		Key:   req.Key,
		Value: req.Value,
	}
	_, err := h.s.Submit(cmd)
	resp.Ok = (err == nil)
	return err
}

func (h *Handler) Get(req *GetRequest, resp *GetResponse) error {
	cmd := kv.Command{
		Type: kv.CmdGet,
		Key:  req.Key,
	}

	val, err := h.s.Submit(cmd)
	resp.Value = val
	return err
}

// RPC: Delete
func (h *Handler) Delete(req *DeleteRequest, resp *DeleteResponse) error {
	cmd := kv.Command{
		Type: kv.CmdDelete,
		Key:  req.Key,
	}
	_, err := h.s.Submit(cmd)
	resp.Ok = (err == nil)
	return err
}

func (h *Handler) Watch(req *WatchRequest, resp *WatchResponse) error {
	timeout := time.Duration(req.Timeout) * time.Millisecond
	if req.Timeout == 0 {
		timeout = 0
	}
	watcher := h.s.watchers.AddWatcher(req.Key, req.Persistent, timeout)
	resp.WatcherID = watcher.ID
	return nil
}

func (h *Handler) CancelWatch(req *WatchRequest, resp *WatchResponse) error {
	h.s.CancelWatcher(req.Key, req.WatcherID)
	return nil
}

// HTTP PUT handler
func (h *HTTPHandler) Put(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key := r.URL.Query().Get("key")
	value := r.URL.Query().Get("value")

	if key == "" || value == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"ok":false,"error":"key and value required"}`)
		return
	}
	log.Printf("key is %s,value is %s", key, value)
	cmd := kv.Command{
		Type:  kv.CmdPut,
		Key:   key,
		Value: value,
	}

	_, err := h.server.Submit(cmd)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"ok":false,"error":"%s"}`, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"ok":true}`)
}

// HTTP GET handler
func (h *HTTPHandler) Get(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"ok":false,"error":"key required"}`)
		return
	}

	value := h.server.Get(key)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"ok":true,"value":"%s"}`, value)
}

// HTTP DELETE handler
func (h *HTTPHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"ok":false,"error":"key required"}`)
		return
	}

	cmd := kv.Command{
		Type: kv.CmdDelete,
		Key:  key,
	}

	_, err := h.server.Submit(cmd)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"ok":false,"error":"%s"}`, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"ok":true}`)
}

// Health check
func (h *HTTPHandler) Health(w http.ResponseWriter, r *http.Request) {
	isLeader := h.server.IsLeader()
	status := "follower"
	if isLeader {
		status = "leader"
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"%s","id":%d}`, status, h.server.id)
}

// Stats handler
func (h *HTTPHandler) Stats(w http.ResponseWriter, r *http.Request) {
	stats := h.server.GetStats()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"ok":true,"stats":%s}`, toJSON(stats))
}

func (h *HTTPHandler) Watch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !h.server.IsLeader() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"ok":false,"error":"watch only supported on leader"}`)
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"ok":false,"error":"key required"}`)
		return
	}

	persistent := r.URL.Query().Get("persistent") == "true"
	timeout := r.URL.Query().Get("timeout")

	var timeoutDur time.Duration
	if timeout != "" {
		var timeoutMs int64
		fmt.Sscanf(timeout, "%d", &timeoutMs)
		timeoutDur = time.Duration(timeoutMs) * time.Millisecond
	}

	watcher := h.server.watchers.AddWatcher(key, persistent, timeoutDur)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"ok":true,"watcher_id":%d}`, watcher.ID)
}

// Wait handler - 等待 watch 事件
func (h *HTTPHandler) Wait(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !h.server.IsLeader() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"ok":false,"error":"wait only supported on leader"}`)
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"ok":false,"error":"key required"}`)
		return
	}

	timeout := r.URL.Query().Get("timeout")
	var timeoutDur time.Duration = 5 * time.Second
	if timeout != "" {
		var timeoutMs int64
		fmt.Sscanf(timeout, "%d", &timeoutMs)
		if timeoutMs > 0 {
			timeoutDur = time.Duration(timeoutMs) * time.Millisecond
		}
	}

	persistent := r.URL.Query().Get("persistent") == "true"

	watcher := h.server.watchers.AddWatcher(key, persistent, timeoutDur)

	select {
	case event := <-watcher.Ch:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"ok":true,"key":"%s","value":"%s","type":"%s"}`, event.Key, event.Value, event.Type)
	case <-time.After(timeoutDur):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"ok":false,"error":"timeout"}`)
	}
}

// 简单的JSON编码
func toJSON(m map[string]interface{}) string {
	result := "{"
	for k, v := range m {
		if result != "{" {
			result += ","
		}
		switch val := v.(type) {
		case string:
			result += fmt.Sprintf(`"%s":"%s"`, k, val)
		case int:
			result += fmt.Sprintf(`"%s":%d`, k, val)
		case int64:
			result += fmt.Sprintf(`"%s":%d`, k, val)
		case bool:
			result += fmt.Sprintf(`"%s":%t`, k, val)
		}
	}
	result += "}"
	return result
}

func (h *HTTPHandler) LeaseGrant(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ttl := r.URL.Query().Get("ttl")
	var ttlVal int64 = 10
	if ttl != "" {
		fmt.Sscanf(ttl, "%d", &ttlVal)
	}
	cmd := kv.Command{Type: kv.CmdLeaseGrant, TTL: ttlVal}
	result, err := h.server.Submit(cmd)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"ok":false,"error":"%s"}`, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"ok":true,"lease_id":%s}`, result)
}

func (h *HTTPHandler) LeaseRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	leaseIdStr := r.URL.Query().Get("lease_id")
	var leaseId int64
	if leaseIdStr != "" {
		fmt.Sscanf(leaseIdStr, "%d", &leaseId)
	}
	cmd := kv.Command{Type: kv.CmdLeaseRevoke, LeaseID: leaseId}
	_, err := h.server.Submit(cmd)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"ok":false,"error":"%s"}`, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"ok":true}`)
}

func (h *HTTPHandler) LeaseKeepAlive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	leaseIdStr := r.URL.Query().Get("lease_id")
	var leaseId int64
	if leaseIdStr != "" {
		fmt.Sscanf(leaseIdStr, "%d", &leaseId)
	}
	ttlStr := r.URL.Query().Get("ttl")
	var ttlVal int64 = 10
	if ttlStr != "" {
		fmt.Sscanf(ttlStr, "%d", &ttlVal)
	}
	cmd := kv.Command{Type: kv.CmdLeaseKeepAlive, LeaseID: leaseId, TTL: ttlVal}
	result, err := h.server.Submit(cmd)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"ok":false,"error":"%s"}`, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"ok":true,"result":"%s"}`, result)
}

func (h *HTTPHandler) LeaseAttach(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key := r.URL.Query().Get("key")
	value := r.URL.Query().Get("value")
	leaseIdStr := r.URL.Query().Get("lease_id")
	var leaseId int64
	if leaseIdStr != "" {
		fmt.Sscanf(leaseIdStr, "%d", &leaseId)
	}
	if key == "" || leaseId <= 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"ok":false,"error":"key and lease_id required"}`)
		return
	}

	cmd := kv.Command{Type: kv.CmdLeaseAttach, Key: key, Value: value, LeaseID: leaseId}
	_, err := h.server.Submit(cmd)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"ok":false,"error":"%s"}`, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"ok":true}`)
}
