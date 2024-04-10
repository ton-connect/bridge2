package bridge

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
)

func NewWebhook(url, auth string, workers, queueSize int) chan<- WebhookData {
	cli := http.Client{
		Timeout: 3 * time.Second,
	}

	ch := make(chan WebhookData, queueSize)
	for i := 0; i < workers; i++ {
		go func() {
			for data := range ch {
				postBody, err := json.Marshal(data)
				if err != nil {
					log.Warn().Str("url", url).Str("client_id", data.ClientID).Msg("failed to marshal webhook body")
					continue
				}

				req, err := http.NewRequest(http.MethodPost, url+"/"+hex.EncodeToString([]byte(data.ClientID)), bytes.NewReader(postBody))
				if err != nil {
					log.Warn().Str("url", url).Str("client_id", data.ClientID).Msg("failed to build webhook request")
					continue
				}

				if auth != "" {
					req.Header.Set("Authorization", "Bearer "+auth)
				}
				req.Header.Set("Content-Type", "application/json")
				res, err := cli.Do(req)
				if err != nil {
					log.Warn().Str("url", url).Str("client_id", data.ClientID).Msg("failed to send webhook")
					continue
				}
				_ = res.Body.Close()

				if res.StatusCode != http.StatusOK {
					log.Warn().Str("url", url).Str("client_id", data.ClientID).Int("status", res.StatusCode).Msg("bad response status from webhook")
					continue
				}
				log.Debug().Str("url", url).Str("client_id", data.ClientID).Int("status", res.StatusCode).Msg("webhook sent")
			}
		}()
	}

	return ch
}
