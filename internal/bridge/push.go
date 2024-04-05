package bridge

import (
	"encoding/base64"
	"github.com/rs/zerolog/log"
	"github.com/valyala/fasthttp"
	"time"
	"tonconnect-bridge/internal/bridge/metrics"
)

func (s *SSE) handlePush(ctx *fasthttp.RequestCtx, ip string, authorized bool) {
	if !ctx.IsPost() {
		respError(ctx, "request method is not supported", 400)
		return
	}

	if !authorized && s.pushLimiter.Add(ip, 1) < 1 {
		respError(ctx, "too many push operations per second", 429)
		return
	}

	clientId := string(ctx.QueryArgs().Peek("client_id"))
	if clientId == "" || len(clientId) > 64 {
		respError(ctx, "invalid client_id", 400)
		return
	}

	to := string(ctx.QueryArgs().Peek("to"))
	if to == "" || len(to) > 64 {
		respError(ctx, "invalid to", 400)
		return
	}

	ttl, err := ctx.QueryArgs().GetUint("ttl")
	if err != nil {
		respError(ctx, "ttl is invalid", 400)
		return
	}

	if ttl > s.MaxTTL {
		respError(ctx, "ttl is too long", 400)
		return
	}

	body := ctx.PostBody()
	decodedLen := base64.StdEncoding.DecodedLen(len(body))
	if decodedLen > 64*1024 {
		respError(ctx, "too big message", 400)
		return
	}

	// decode to validate and decrease size
	decoded := make([]byte, decodedLen)
	if _, err = base64.StdEncoding.Decode(decoded, body); err != nil {
		respError(ctx, "invalid payload", 400)
		return
	}

	cli := s.client(to, false)

	tm := time.Now()
	added := cli.Push(&Event{
		ID:       uint64(tm.UnixNano()),
		Message:  decoded,
		Deadline: tm.Unix() + int64(ttl),
	})
	if !added {
		log.Debug().Str("id", to).Msg("client buffer overflow")
		respError(ctx, "client's buffer overflow", 403)
		return
	}

	select {
	case cli.Signal <- struct{}{}:
	default:
	}

	if topic := string(ctx.QueryArgs().Peek("topic")); topic != "" {
		wh := WebhookData{
			ClientID: clientId,
			Topic:    topic,
			Hash:     decoded,
		}

		for i, webhook := range s.webhooks {
			select {
			case webhook <- wh:
				log.Debug().Int("index", i).Str("topic", topic).Msg("webhook added to queue")
			default:
				// skip when overflow
				log.Warn().Int("index", i).Str("topic", topic).Msg("webhook buffer overflow")
			}
		}
	}

	metrics.Global.PushedMessages.Inc()
	respOk(ctx)
	return
}
