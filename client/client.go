package client

import (
	"fmt"
	"net/rpc"
	"testetcd/server"
)

type Client struct {
	addr   []string
	conn   *rpc.Client
	leader string
}

type Watch struct {
	Key      string
	revision int //每一次写操作的一个全局递增号
}

func NewClient(addr string) (*Client, error) {
	conn, err := rpc.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn}, nil
}

func (c *Client) Put(key, value string) error {
	req := &server.PutRequest{Key: key, Value: value}
	resp := &server.PutResponse{}

	err := c.conn.Call("Handler.Put", req, resp)
	if err != nil {
		return err
	}
	if !resp.Ok {
		return fmt.Errorf("put failed")
	}
	return nil
}

func (c *Client) Get(key string) (string, error) {
	req := &server.GetRequest{Key: key}
	resp := &server.GetResponse{}

	err := c.conn.Call("Handler.Get", req, resp)
	if err != nil {
		return "", err
	}
	return resp.Value, nil
}

func (c *Client) Delete(key string) error {
	req := &server.DeleteRequest{Key: key}
	resp := &server.DeleteResponse{}

	err := c.conn.Call("Handler.Delete", req, resp)
	if err != nil {
		return err
	}
	if !resp.Ok {
		return fmt.Errorf("delete failed")
	}
	return nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) Watch(key string, persistent bool, timeoutMs int64) (uint64, error) {
	req := &server.WatchRequest{
		Key:        key,
		Persistent: persistent,
		Timeout:    timeoutMs,
	}
	resp := &server.WatchResponse{}

	err := c.conn.Call("Handler.Watch", req, resp)
	if err != nil {
		return 0, err
	}
	return resp.WatcherID, nil
}

func (c *Client) CancelWatch(key string, watcherID uint64) error {
	req := &server.WatchRequest{
		Key:       key,
		WatcherID: watcherID,
	}
	resp := &server.WatchResponse{}

	err := c.conn.Call("Handler.CancelWatch", req, resp)
	return err
}
