package service

import (
	"context"
	"net/http"
	"time"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
)

// Refresh 重新触发一次解析：已 pending/processing 的链接直接复用在飞任务，
// 已 done/failed 的链接在冷却窗内会返回 429（带 Retry-After），冷却结束后
// 才把状态置回 pending 并入新队列任务。冷却用链接最近一次采集时间作锚点，
// 防止脚本无限循环刷新。
func (s *SubmitService) Refresh(ctx context.Context, linkID string) (dto.SubmitResponse, error) {
	link, err := s.core.requireExisting(ctx, linkID)
	if err != nil {
		return dto.SubmitResponse{}, err
	}

	var resp dto.SubmitResponse
	err = s.core.locker.WithURL(ctx, link.URL, func(lockCtx context.Context) error {
		current, err := s.core.requireExisting(lockCtx, linkID)
		if err != nil {
			return err
		}
		if current.Status == model.LinkStatusPending || current.Status == model.LinkStatusProcessing {
			resp = dto.SubmitResponse{
				LinkID: current.ID.String(),
				Status: string(current.Status),
			}
			return nil
		}

		// Cooldown: when the link is already terminal (done/failed),
		// reject a follow-up Refresh that lands inside the per-link
		// window. ParseRequestedAt is derived from the Link's existing
		// first/last collection timestamps and advances on every successful
		// requeue. The 429 carries a
		// Retry-After hint so well-behaved clients can back off
		// without polling.
		if !current.ParseRequestedAt.IsZero() && s.refreshCooldown > 0 {
			now := s.now
			if now == nil {
				now = time.Now
			}
			elapsed := now().Sub(current.ParseRequestedAt)
			if elapsed >= 0 && elapsed < s.refreshCooldown {
				retry := int((s.refreshCooldown - elapsed + time.Second - 1) / time.Second)
				if retry < 1 {
					retry = 1
				}
				// 带 slug：客户端用 cooldown_active 给出针对性的重新解析提示，
				// 不必从通用 429 文案推断业务状态。
				// （"x 秒后可重试"而非"请稍后重试"）。
				return httperr.NewWithCodeAndRetryAfter(http.StatusTooManyRequests, httperr.CodeCooldownActive, "refresh cooldown active", retry)
			}
		}

		resp, err = s.core.requeueExisting(lockCtx, current.ID, nil)
		return err
	})
	return resp, err
}
