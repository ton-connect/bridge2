package bridge

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/kevinms/leakybucket-go"
	"github.com/rs/zerolog/log"
	"github.com/valyala/fasthttp"
	"tonconnect-bridge/internal/bridge/metrics"
)

type Store interface {
	Push(e *Event) bool
	ExecuteAll(lastEventId uint64, exec func(event *Event) error) error
}

type IP struct {
	ActiveConnections int32
}

type Event struct {
	ID       uint64 `json:"-"`
	Deadline int64  `json:"-"`

	From    string `json:"from"`
	Message []byte `json:"message"`
}

type WebhookData struct {
	ClientID string `json:"-"`
	Topic    string `json:"topic"`
	Hash     []byte `json:"hash"`
}

type Resp struct {
	Message    string `json:"message"`
	StatusCode int    `json:"statusCode"` // ??? backwards compatibility
}

type Client struct {
	Store
	Signal        chan struct{}
	LastUsed      int64
	Subscriptions int32
}

type SSEConfig struct {
	EnableCORS             bool
	MaxConnectionsPerIP    int32
	MaxTTL                 int
	RateLimitIgnoreTokens  []string
	MaxClientsPerSubscribe int
	MaxPushesPerSec        float64
	HeartbeatSeconds       int
	HeartbeatGroups        int
}

type SSE struct {
	ips         map[string]*IP
	pushLimiter *leakybucket.Collector
	clients     map[string]*Client
	pingWaiters []unsafe.Pointer

	storageMaker func(id string) Store
	webhooks     []chan<- WebhookData
	clientMx     sync.Mutex
	ipMx         sync.Mutex

	connectionsIter uint64
	SSEConfig
}

func NewSSE(storageMaker func(id string) Store, webhooks []chan<- WebhookData, config SSEConfig) *SSE {
	sse := &SSE{
		ips:          map[string]*IP{},
		storageMaker: storageMaker,
		webhooks:     webhooks,
		clients:      map[string]*Client{},
		pushLimiter:  leakybucket.NewCollector(config.MaxPushesPerSec, int64(config.MaxPushesPerSec*10), true),
		SSEConfig:    config,
	}

	for i := 0; i < config.HeartbeatGroups; i++ {
		ch := make(chan struct{})
		sse.pingWaiters = append(sse.pingWaiters, unsafe.Pointer(&ch))
	}
	go sse.pingWorker()
	go sse.cleanerWorker()
	go sse.refreshUnlimitedTokens()

	return sse
}

func (s *SSE) pingWorker() {
	// ping all client groups
	perGroup := (time.Duration(s.HeartbeatSeconds) * time.Second) / time.Duration(s.HeartbeatGroups)
	for {
		for i := 0; i < s.HeartbeatGroups; i++ {
			ch := make(chan struct{})
			old := (*chan struct{})(atomic.LoadPointer(&s.pingWaiters[i]))
			atomic.StorePointer(&s.pingWaiters[i], unsafe.Pointer(&ch))
			close(*old)

			time.Sleep(perGroup)
		}
	}
}

func (s *SSE) cleanerWorker() {
	for {
		time.Sleep(5 * time.Second)

		old := time.Now().Unix() - int64(s.MaxTTL)

		s.clientMx.Lock()
		for k, c := range s.clients {
			if atomic.LoadInt32(&c.Subscriptions) == 0 && atomic.LoadInt64(&c.LastUsed) < old {
				delete(s.clients, k)
			}
		}
		s.clientMx.Unlock()
	}
}

func (s *SSE) refreshUnlimitedTokens() {
	type UnlimitedTokens struct {
		Tokens []string `json:"tokens"`
	}
	refresh := func() []string {
		file, err := os.ReadFile("unlimited_tokens.json")
		if err != nil {
			log.Printf("failed to read unlimited tokens file: %v", err)
			return []string{}
		}
		var tokens UnlimitedTokens
		if err = json.Unmarshal(file, &tokens); err != nil {
			log.Printf("failed to convert unlimited tokens: %v", err)
			return []string{}
		}
		return tokens.Tokens
	}
	for {
		tokens := refresh()
		if len(tokens) != 0 {
			s.RateLimitIgnoreTokens = tokens
		}
		time.Sleep(time.Minute * 5)
	}
}

func (s *SSE) Handle(ctx *fasthttp.RequestCtx) {
	var authorized bool
	if auth := string(ctx.Request.Header.Peek("Authorization")); strings.HasPrefix(auth, "Bearer ") {
		authorized = slices.Contains(s.RateLimitIgnoreTokens, auth[7:])
	}

	if s.EnableCORS {
		origin := string(ctx.Request.Header.Peek("Origin"))
		if origin == "" {
			origin = "*"
		}

		ctx.Response.Header.Set("Access-Control-Allow-Origin", origin)
		ctx.Response.Header.Set("Access-Control-Allow-Credentials", "true")
	}

	switch {
	case ctx.IsGet() || ctx.IsPost():
	case s.EnableCORS && ctx.IsOptions():
		ctx.Response.Header.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		ctx.Response.Header.Set("Access-Control-Max-Age", "86400")
		ctx.Response.Header.Set("Access-Control-Allow-Headers", strings.Join([]string{
			"DNT", "X-CustomHeader", "Keep-Alive", "User-Agent", "X-Requested-With", "If-Modified-Since", "Cache-Control", "Content-Type", "Authorization", "Origin",
		}, ","))

		ctx.SetStatusCode(204)
		return
	default:
		respErrorNoMetric(ctx, "incorrect request type", 400)
		return
	}

	switch string(ctx.Path()) {
	case "/bridge/events":
		s.handleSubscribe(ctx, realIP(ctx), authorized)
	case "/bridge/message":
		s.handlePush(ctx, realIP(ctx), authorized)
	default:
		respErrorNoMetric(ctx, "not found", 404)
	}
}

func realIP(ctx *fasthttp.RequestCtx) string {
	// backwards compatibility
	// TODO: use headers only if behind balancer
	if ip := string(ctx.Request.Header.Peek("X-Forwarded-For")); ip != "" {
		i := strings.IndexAny(ip, ",")
		if i > 0 {
			return strings.Trim(ip[:i], "[] \t")
		}
		return ip
	}
	if ip := string(ctx.Request.Header.Peek("X-Real-Ip")); ip != "" {
		return strings.Trim(ip, "[]")
	}
	ra, _, _ := net.SplitHostPort(ctx.RemoteAddr().String())
	return ra
}

func (s *SSE) client(shortedId string, subscribed bool) *Client {
	s.clientMx.Lock()
	defer s.clientMx.Unlock()

	cli := s.clients[shortedId]
	if cli == nil {
		cli = &Client{
			Store:  s.storageMaker(shortedId),
			Signal: make(chan struct{}, 1),
		}
		s.clients[shortedId] = cli
	}

	if subscribed {
		atomic.AddInt32(&cli.Subscriptions, 1)
	}

	atomic.StoreInt64(&cli.LastUsed, time.Now().Unix())
	return cli
}

func respError(ctx *fasthttp.RequestCtx, msg string, code int) {
	metrics.Global.Requests.WithLabelValues(string(ctx.Path()),
		fmt.Sprint(code)).Observe(time.Since(ctx.Time()).Seconds())

	respErrorNoMetric(ctx, msg, code)
}

func respErrorNoMetric(ctx *fasthttp.RequestCtx, msg string, code int) {
	data, _ := json.Marshal(Resp{
		Message:    msg,
		StatusCode: code,
	})
	ctx.SetContentType("application/json")
	ctx.SetStatusCode(code)
	_, _ = ctx.Write(data)
}

func respOk(ctx *fasthttp.RequestCtx) {
	metrics.Global.Requests.WithLabelValues(string(ctx.Path()),
		fmt.Sprint(200)).Observe(time.Since(ctx.Time()).Seconds())

	data, _ := json.Marshal(Resp{
		Message:    "OK",
		StatusCode: 200,
	})
	ctx.SetContentType("application/json")
	ctx.SetStatusCode(200)
	_, _ = ctx.Write(data)
}
