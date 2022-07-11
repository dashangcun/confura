package middlewares

import (
	"context"
	"errors"

	"github.com/conflux-chain/conflux-infura/util/rpc/handlers"
	"github.com/openweb3/go-rpc-provider"
)

var errRateLimit = errors.New("too many requests")

func RateLimitBatch(next rpc.HandleBatchFunc) rpc.HandleBatchFunc {
	return func(ctx context.Context, msgs []*rpc.JsonRpcMessage) []*rpc.JsonRpcMessage {
		if handlers.WhiteListAllow(ctx) || handlers.RateLimitAllow(ctx, "rpc_batch", len(msgs)) {
			return next(ctx, msgs)
		}

		var responses []*rpc.JsonRpcMessage
		for _, v := range msgs {
			responses = append(responses, v.ErrorResponse(errRateLimit))
		}

		return responses
	}
}

func RateLimit(next rpc.HandleCallMsgFunc) rpc.HandleCallMsgFunc {
	return func(ctx context.Context, msg *rpc.JsonRpcMessage) *rpc.JsonRpcMessage {
		// white list allow?
		if handlers.WhiteListAllow(ctx) {
			return next(ctx, msg)
		}

		// overall rate limit
		if !handlers.RateLimitAllow(ctx, "rpc_all", 1) {
			return msg.ErrorResponse(errRateLimit)
		}

		// single method rate limit
		if !handlers.RateLimitAllow(ctx, msg.Method, 1) {
			return msg.ErrorResponse(errRateLimit)
		}

		return next(ctx, msg)
	}
}