package server

import (
	"fmt"
	"net/http"
	"testetcd/kv"
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

// 简单的JSON编码
func toJSON(m map[string]interface{}) string {
	// 简化实现，实际应该用json.Marshal
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
		case bool:
			result += fmt.Sprintf(`"%s":%t`, k, val)
		}
	}
	result += "}"
	return result
}
