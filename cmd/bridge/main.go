package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/valyala/fasthttp"
	"io"
	"math/big"
	"net/http"
	"os"
	"time"
	"tonconnect-bridge/internal/bridge"
	"tonconnect-bridge/internal/store"
)

var (
	verbosity       = flag.Int("v", 2, "3 = debug, 2 = info, 1 = warn, 0 = error")
	addr            = flag.String("addr", ":8080", "TCP address to listen to")
	metricsAddr     = flag.String("metrics-addr", ":8081", "Metrics TCP address to listen to")
	tls             = flag.Bool("tls", false, "Enable self signed tls")
	cors            = flag.Bool("cors", false, "Enable CORS")
	jsonLogs        = flag.Bool("json-logs", false, "JSON logs output")
	hb              = flag.Uint("hb", 10, "Heartbeat every seconds")
	ttl             = flag.Uint("ttl", 300, "Max message ttl")
	hbGroups        = flag.Uint("hb-groups", 10, "Heartbeat groups (shards)")
	pushRPS         = flag.Uint("push-limit", 5, "Push RPS limit")
	subLimit        = flag.Uint("subscribe-limit", 100, "Parallel subscriptions per IP limit")
	subClientsLimit = flag.Uint("max-subscribe-clients", 100, "Clients limit per subscription")
	limitsSkipToken = flag.String("bypass-token", "", "Limits bypass token")
	webhook         = flag.String("webhook", "", "Webhook URL")
	webhookAuth     = flag.String("webhook-auth", "", "Bearer token which will be sent in Authorization header of webhook")
)

func main() {
	flag.Parse()

	var logWr io.Writer = zerolog.NewConsoleWriter()
	if *jsonLogs {
		logWr = os.Stderr
	}
	log.Logger = zerolog.New(logWr).With().Timestamp().Logger().Level(zerolog.InfoLevel)

	switch *verbosity {
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
	if *webhook != "" {
		webhooks = append(webhooks, bridge.NewWebhook(*webhook, *webhookAuth, 8, 512))
	}

	if *subClientsLimit > 65000 {
		panic("too many clients per subscription")
	}

	sse := bridge.NewSSE(maker, webhooks, bridge.SSEConfig{
		EnableCORS:             *cors,
		MaxConnectionsPerIP:    int32(*subLimit),
		MaxTTL:                 int(*ttl),
		RateLimitIgnoreToken:   *limitsSkipToken,
		MaxClientsPerSubscribe: int(*subClientsLimit),
		MaxPushesPerSec:        float64(*pushRPS),
		HeartbeatSeconds:       int(*hb),
		HeartbeatGroups:        int(*hbGroups),
	})

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		if err := http.ListenAndServe(*metricsAddr, nil); err != nil {
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
		ReadBufferSize:        1024 + int(*subClientsLimit)*65, // because many client_ids can be passed
	}

	if *tls {
		cert, key, err := generateSelfSignedCertificate()
		if err != nil {
			log.Fatal().Err(err).Msg("failed generateSelfSignedCertificate")
		}

		if err = srv.ListenAndServeTLSEmbed(*addr, cert, key); err != nil {
			log.Fatal().Err(err).Msg("failed ListenAndServeTLSEmbed")
		}
		return
	}

	if err := srv.ListenAndServe(*addr); err != nil {
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
