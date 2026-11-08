package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Минимальный клиент WebSocket (RFC 6455) — ровно столько, сколько нужно для
// протокола отладки Chrome на 127.0.0.1: текстовые кадры, фрагментация,
// ping/pong, close. Шифрования нет: соединение локальное.

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func wsDial(rawURL string, timeout time.Duration) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("only ws:// is supported, got %s", u.Scheme)
	}
	c, err := net.DialTimeout("tcp", u.Host, timeout)
	if err != nil {
		return nil, err
	}
	keyRaw := make([]byte, 16)
	_, _ = rand.Read(keyRaw)
	key := base64.StdEncoding.EncodeToString(keyRaw)
	req := "GET " + u.RequestURI() + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	_ = c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write([]byte(req)); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReaderSize(c, 1<<16)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		c.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		c.Close()
		return nil, fmt.Errorf("websocket handshake: HTTP %d", resp.StatusCode)
	}
	h := sha1.Sum([]byte(key + wsGUID))
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(h[:]) {
		c.Close()
		return nil, errors.New("websocket handshake: bad Sec-WebSocket-Accept")
	}
	_ = c.SetDeadline(time.Time{})
	return &wsConn{conn: c, br: br}, nil
}

func (w *wsConn) writeFrame(op byte, payload []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	var hdr []byte
	hdr = append(hdr, 0x80|op)
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 1<<16:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		hdr = append(hdr, b[:]...)
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	hdr = append(hdr, mask[:]...)
	buf := make([]byte, len(hdr)+n)
	copy(buf, hdr)
	for i := 0; i < n; i++ {
		buf[len(hdr)+i] = payload[i] ^ mask[i%4]
	}
	_, err := w.conn.Write(buf)
	return err
}

func (w *wsConn) WriteText(b []byte) error { return w.writeFrame(0x1, b) }

// ReadMessage возвращает следующее текстовое или двоичное сообщение целиком.
func (w *wsConn) ReadMessage() ([]byte, error) {
	var msg []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(w.br, h[:]); err != nil {
			return nil, err
		}
		fin := h[0]&0x80 != 0
		op := h[0] & 0x0f
		masked := h[1]&0x80 != 0
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(w.br, b[:]); err != nil {
				return nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(w.br, b[:]); err != nil {
				return nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if n > 256<<20 {
			return nil, fmt.Errorf("websocket frame too large: %d bytes", n)
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(w.br, mask[:]); err != nil {
				return nil, err
			}
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(w.br, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch op {
		case 0x8: // close
			_ = w.writeFrame(0x8, nil)
			return nil, io.EOF
		case 0x9: // ping
			_ = w.writeFrame(0xA, payload)
			continue
		case 0xA: // pong
			continue
		}
		msg = append(msg, payload...)
		if fin {
			return msg, nil
		}
	}
}

func (w *wsConn) Close() error {
	_ = w.writeFrame(0x8, nil)
	return w.conn.Close()
}

func isLocalWS(u string) bool {
	return strings.HasPrefix(u, "ws://127.0.0.1:") || strings.HasPrefix(u, "ws://localhost:")
}
