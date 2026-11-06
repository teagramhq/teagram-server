package api

import "github.com/teagramhq/teagram-server/internal/mtproto"

const channelUnreadCountRateLimitSurface = "channel_unread_count"

func (h *handlers) checkChannelUnreadCountRateLimit(req *mtproto.Request) error {
	return h.checkRateLimit(req, channelUnreadCountRateLimitSurface, h.rateLimitChannelUnreadCounts)
}
