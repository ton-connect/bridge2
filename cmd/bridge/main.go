package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/sethvargo/go-envconfig"
	"github.com/valyala/fasthttp"
	"tonconnect-bridge/internal/bridge"
	"tonconnect-bridge/internal/store"
)

type Config struct {
	Verbosity                 int    `env:"VERBOSITY, default=2"`                      // 3 = debug, 2 = info, 1 = warn, 0 = error
	Addr                      string `env:"LISTEN_ADDR, default=:8080"`                // TCP address to listen to
	MetricsAddr               string `env:"METRICS_ADDR, default=:8081"`               // Metrics TCP address to listen to
	TLS                       bool   `env:"TLS, default=false"`                        // Enable self signed tls
	CORS                      bool   `env:"CORS, default=false"`                       // Enable CORS
	JsonLogs                  bool   `env:"JSON_LOGS, default=false"`                  // Enable JSON logs output
	HeartbeatSeconds          uint   `env:"HEARTBEAT_SECONDS, default=10"`             // Heartbeat every seconds
	MaxMessageTTL             uint   `env:"MAX_MESSAGE_TTL, default=300"`              // "Max message ttl
	HeartbeatGroups           uint   `env:"HEARTBEAT_GROUPS, default=10"`              // Heartbeat groups (shards)
	PushRPS                   uint   `env:"PUSH_RPS_LIMIT, default=5"`                 // Push RPS limit
	MaxSubscribersPerIP       uint   `env:"MAX_SUBSCRIBERS_PER_IP, default=100"`       // Parallel subscriptions per IP limit
	MaxClientsPerSubscription uint   `env:"MAX_CLIENTS_PER_SUBSCRIPTION, default=100"` // Clients limit per subscription
	BypassToken               string `env:"LIMITS_BYPASS_TOKEN"`
	WebhookURL                string `env:"WEBHOOK_URL"`
	WebhookAuth               string `env:"WEBHOOK_AUTH"` // Bearer token which will be sent in Authorization header of webhook
}

func main() {
	var cfg Config
	if err := envconfig.Process(context.Background(), &cfg); err != nil {
		panic("failed to process env: " + err.Error())
	}

	var logWr io.Writer = zerolog.NewConsoleWriter()
	if cfg.JsonLogs {
		logWr = os.Stderr
	}
	log.Logger = zerolog.New(logWr).With().Timestamp().Logger().Level(zerolog.InfoLevel)

	switch cfg.Verbosity {
	case 3:
		log.Logger = log.Logger.Level(zerolog.DebugLevel).With().Logger()
	case 2:
		log.Logger = log.Logger.Level(zerolog.InfoLevel).With().Logger()
	case 1:
		log.Logger = log.Logger.Level(zerolog.WarnLevel).With().Logger()
	case 0:
		log.Logger = log.Logger.Level(zerolog.ErrorLevel).With().Logger()
	}

	// basic mem store creator
	maker := func(id string) bridge.Store {
		return &store.Memory{}
	}

	var webhooks []chan<- bridge.WebhookData
	if cfg.WebhookURL != "" {
		webhooks = append(webhooks, bridge.NewWebhook(cfg.WebhookURL, cfg.WebhookAuth, 8, 512))
	}

	if cfg.MaxClientsPerSubscription > 65000 {
		panic("too many clients per subscription")
	}

	sse := bridge.NewSSE(maker, webhooks, bridge.SSEConfig{
		EnableCORS:             cfg.CORS,
		MaxConnectionsPerIP:    int32(cfg.MaxSubscribersPerIP),
		MaxTTL:                 int(cfg.MaxMessageTTL),
		RateLimitIgnoreToken:   cfg.BypassToken,
		MaxClientsPerSubscribe: int(cfg.MaxClientsPerSubscription),
		MaxPushesPerSec:        float64(cfg.PushRPS),
		HeartbeatSeconds:       int(cfg.HeartbeatSeconds),
		HeartbeatGroups:        int(cfg.HeartbeatGroups),
	})

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		if err := http.ListenAndServe(cfg.MetricsAddr, nil); err != nil {
			log.Fatal().Err(err).Msg("listen metrics failed")
		}
	}()

	log.Info().Msg("bridge server started")

	srv := &fasthttp.Server{
		Handler:               sse.Handle,
		Concurrency:           2000000,
		NoDefaultServerHeader: true,
		NoDefaultDate:         true,
		WriteBufferSize:       1024,
		ReadBufferSize:        2048 + int(cfg.MaxClientsPerSubscription)*65, // because many client_ids can be passed
		ReduceMemoryUsage:     true,                                         // to clean up read buffers and not hold them
	}

	if cfg.TLS {
		cert, key, err := generateSelfSignedCertificate()
		if err != nil {
			log.Fatal().Err(err).Msg("failed generateSelfSignedCertificate")
		}

		if err = srv.ListenAndServeTLSEmbed(cfg.Addr, cert, key); err != nil {
			log.Fatal().Err(err).Msg("failed ListenAndServeTLSEmbed")
		}
		return
	}

	if err := srv.ListenAndServe(cfg.Addr); err != nil {
		log.Fatal().Err(err).Msg("failed ListenAndServe")
	}
}

func generateSelfSignedCertificate() ([]byte, []byte, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, nil, err
	}

	certTemplate := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"TON"},
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(1000 * 24 * time.Hour),

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &certTemplate, &certTemplate, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	key, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return nil, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: key})

	return certPEM, keyPEM, nil
}
