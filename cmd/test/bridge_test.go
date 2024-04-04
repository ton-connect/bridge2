package test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/valyala/fasthttp"
	"io"
	"math/rand"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"
	"tonconnect-bridge/internal/bridge"
	"tonconnect-bridge/internal/store"
)

type fakeConn struct {
	clientIds []string

	addr    net.Addr
	rch     chan []byte
	rchRest chan []byte

	wch    chan []byte
	closed bool
}

func newFakeConn(addr net.Addr, w chan []byte) *fakeConn {
	return &fakeConn{
		addr:    addr,
		rch:     make(chan []byte, 10),
		rchRest: make(chan []byte, 1),

		wch: w,
	}
}

func (f *fakeConn) Read(b []byte) (n int, err error) {
	if f.closed {
		return -1, io.ErrClosedPipe
	}

	var buf []byte
	select {
	case buf = <-f.rchRest:
		// first read prev unread
	case buf = <-f.rch:
	}

	if buf == nil {
		return -1, io.ErrClosedPipe
	}

	sz := copy(b, buf)
	buf = buf[sz:]

	if len(buf) > 0 {
		f.rchRest <- buf
	}
	return sz, nil
}

func (f *fakeConn) Write(b []byte) (n int, err error) {
	if f.closed {
		return -1, io.ErrClosedPipe
	}

	f.wch <- b
	return len(b), nil
}

func (f *fakeConn) Close() error {
	f.closed = true
	return nil
}

func (f *fakeConn) LocalAddr() net.Addr {
	return &net.TCPAddr{
		IP:   net.IPv4(1, 1, 1, 1),
		Port: 7070,
	}
}

func (f *fakeConn) RemoteAddr() net.Addr {
	return f.addr
}

func (f *fakeConn) SetDeadline(t time.Time) error {
	return nil
}

func (f *fakeConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (f *fakeConn) SetWriteDeadline(t time.Time) error {
	return nil
}

type listener struct {
	// server conns to accept
	conns chan net.Conn
}

func (l *listener) Accept() (net.Conn, error) {
	conn, ok := <-l.conns
	if !ok {
		return nil, errors.New("listenerDialer is closed")
	}
	return conn, nil
}

func (l *listener) Close() error {
	close(l.conns)
	return nil
}

func (l *listener) Addr() net.Addr {
	// return arbitrary fake addr.
	return &net.UnixAddr{
		Name: "listenerDialer",
		Net:  "fake",
	}
}

func NewListenerDialer() *listener {
	ld := &listener{
		conns: make(chan net.Conn),
	}
	return ld
}

func TestBridge(t *testing.T) {
	ls := &listener{
		conns: make(chan net.Conn),
	}

	log.Logger = zerolog.Nop()

	// basic mem store creator
	maker := func(id string) bridge.Store {
		return &store.Memory{}
	}

	wh := make(chan bridge.WebhookData, 100)
	var whs []chan<- bridge.WebhookData
	whs = append(whs, wh)

	sse := bridge.NewSSE(maker, whs, bridge.SSEConfig{
		MaxConnectionsPerIP:    10,
		MaxTTL:                 300,
		RateLimitIgnoreToken:   "123",
		MaxClientsPerSubscribe: 7,
		MaxPushesPreSec:        5,
		HeartbeatSeconds:       5,
		HeartbeatGroups:        10,
	})

	wCh := make(chan []byte, 10000)

	var packets uint64
	var webhooks uint64
	var subscribers uint64
	var pushes uint64

	const ips = 335000
	const connections = 3
	const subscribeClientsPerConnection = 3

	var fakes = make([]*fakeConn, 0, ips*connections)
	var fakesClients = make([][]string, 0, ips*connections)

	go func() {
		for i := 0; i < ips; i++ {
			bb := make([]byte, 4)
			binary.LittleEndian.PutUint32(bb, uint32(i))
			for y := 0; y < connections; y++ {
				gg := make([]byte, 4)
				binary.LittleEndian.PutUint32(gg, uint32(y))

				fc := newFakeConn(&net.TCPAddr{
					IP:   net.IPv4(bb[0], bb[1], bb[2], bb[3]),
					Port: 30000 + y,
				}, wCh)

				ls.conns <- fc

				var cs []string

				for z := 0; z < subscribeClientsPerConnection; z++ {
					ff := make([]byte, 4)
					binary.LittleEndian.PutUint32(ff, uint32(z))

					hs := sha256.New()
					hs.Write(bb)
					hs.Write(gg)
					hs.Write(ff)
					cs = append(cs, hex.EncodeToString(hs.Sum(nil)))
				}

				subscribe := fmt.Sprintf("GET /bridge/events?client_id=%s&last_event_id=1 HTTP/1.1\r\n\r\n", strings.Join(cs, ","))
				fc.rch <- []byte(subscribe)
				subscribers++

				fakes = append(fakes, fc)
				fakesClients = append(fakesClients, cs)
			}
		}

		fmt.Println("INITIALIZED, STARTING PUSHES")

		const rps = 800
		speed := time.Second / time.Duration(rps)

		// pick random connections and clients and send messages to other random clients
		for {
			tm := time.Now()
			fromId := rand.Int() % len(fakes)
			toId := rand.Int() % len(fakes)

			fromCli := fakesClients[fromId]
			fromCliId := rand.Int() % len(fromCli)

			toCli := fakesClients[toId]
			toCliId := rand.Int() % len(toCli)

			bb := make([]byte, 4)
			binary.LittleEndian.PutUint32(bb, rand.Uint32())

			fc := newFakeConn(&net.TCPAddr{
				IP:   net.IPv4(bb[0], bb[1], bb[2], bb[3]),
				Port: 50000,
			}, wCh)

			ls.conns <- fc

			fc.rch <- []byte("POST /bridge/message?client_id=" + fromCli[fromCliId] + "&to=" + toCli[toCliId] + "&topic=kek&ttl=300 HTTP/1.1\r\nContent-Type: text/plain\r\nContent-Length: 12\r\n\r\naGVsbG8gYnJv")
			pushes++

			took := time.Since(tm)
			if took < speed {
				// wait to keep desired rps
				time.Sleep(speed - took)
			}
		}
	}()

	go func() {
		for e := range wCh {
			_ = e
			packets++
		}
	}()

	go func() {
		for e := range wh {
			_ = e
			webhooks++
		}
	}()

	// stats
	go func() {
		for {
			time.Sleep(1 * time.Second)
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			fmt.Printf("\nAlloc = %v MB", m.Alloc/1024/1024)
			fmt.Printf("\nGoroutines = %v", runtime.NumGoroutine())
			fmt.Printf("\nSubscribers = %v", subscribers)
			fmt.Printf("\nPushes = %v", pushes)
			fmt.Printf("\nWebhooks = %v", webhooks)
			fmt.Printf("\nPacketsSent = %v\n", packets)
		}
	}()

	srv := &fasthttp.Server{
		Handler:               sse.Handle,
		Concurrency:           2000000,
		NoDefaultServerHeader: true,
		NoDefaultDate:         true,
		WriteBufferSize:       1024,
		ReadBufferSize:        1024,
	}
	if err := srv.Serve(ls); err != nil {
		panic(err.Error())
	}
}
