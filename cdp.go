package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Клиент протокола отладки Chrome (CDP) поверх ws.go: команды с ответом по id,
// события — подписчикам. Сессии вкладок — плоские (flatten), sessionId в
// каждом сообщении.

type cdpMsg struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type cdpClient struct {
	ws      *wsConn
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan cdpMsg
	subs    map[int64]func(cdpMsg)
	subID   int64
	done    chan struct{}
	err     error
}

func cdpConnect(wsURL string) (*cdpClient, error) {
	if !isLocalWS(wsURL) {
		return nil, fmt.Errorf("refusing a non-local DevTools endpoint: %s", wsURL)
	}
	ws, err := wsDial(wsURL, 10*time.Second)
	if err != nil {
		return nil, err
	}
	c := &cdpClient{ws: ws, pending: map[int64]chan cdpMsg{}, subs: map[int64]func(cdpMsg){}, done: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

func (c *cdpClient) readLoop() {
	defer close(c.done)
	for {
		b, err := c.ws.ReadMessage()
		if err != nil {
			c.mu.Lock()
			c.err = err
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		var m cdpMsg
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		if m.ID != 0 {
			c.mu.Lock()
			ch := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		c.mu.Lock()
		subs := make([]func(cdpMsg), 0, len(c.subs))
		for _, f := range c.subs {
			subs = append(subs, f)
		}
		c.mu.Unlock()
		for _, f := range subs {
			f(m)
		}
	}
}

func (c *cdpClient) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// subscribe — все события; вернуть функцию отписки.
func (c *cdpClient) subscribe(f func(cdpMsg)) func() {
	c.mu.Lock()
	c.subID++
	id := c.subID
	c.subs[id] = f
	c.mu.Unlock()
	return func() { c.mu.Lock(); delete(c.subs, id); c.mu.Unlock() }
}

var errCDPClosed = errors.New("the browser connection is closed")

func (c *cdpClient) call(ctx context.Context, session, method string, params any, out any) error {
	id := c.nextID.Add(1)
	m := map[string]any{"id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	if session != "" {
		m["sessionId"] = session
	}
	b, _ := json.Marshal(m)
	ch := make(chan cdpMsg, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return errCDPClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.ws.WriteText(b); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return errCDPClosed
		}
		if r.Error != nil {
			return fmt.Errorf("%s: %s", method, r.Error.Message)
		}
		if out != nil && len(r.Result) > 0 {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

func (c *cdpClient) close() { _ = c.ws.Close() }

// evaluate выполняет выражение на вкладке, ждёт промис и возвращает значение как JSON.
func (c *cdpClient) evaluate(ctx context.Context, session, expr string) (json.RawMessage, error) {
	var r struct {
		Result struct {
			Type        string          `json:"type"`
			Value       json.RawMessage `json:"value"`
			Description string          `json:"description"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	err := c.call(ctx, session, "Runtime.evaluate", map[string]any{
		"expression": expr, "awaitPromise": true, "returnByValue": true, "userGesture": true,
	}, &r)
	if err != nil {
		return nil, err
	}
	if r.ExceptionDetails != nil {
		msg := r.ExceptionDetails.Text
		if r.ExceptionDetails.Exception != nil && r.ExceptionDetails.Exception.Description != "" {
			msg = r.ExceptionDetails.Exception.Description
		}
		return nil, errors.New(msg)
	}
	if len(r.Result.Value) == 0 {
		return json.RawMessage("null"), nil
	}
	return r.Result.Value, nil
}
