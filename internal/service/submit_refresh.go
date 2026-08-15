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
// 才把状态置回 pending 并入新队列任务。冷却用任务 created_at 当时间锚点，
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
		latest, err := s.core.jobs.GetLatestByLinkID(lockCtx, current.ID)
		if err != nil {
			return err
		}
		if (current.Status == model.LinkStatusPending || current.Status == model.LinkStatusProcessing) && latest != nil {
			jobIDStr := latest.ID.String()
			resp = dto.SubmitResponse{
				JobID:  &jobIDStr,
				LinkID: current.ID.String(),
				Status: string(current.Status),
			}
			return nil
		}

		// Cooldown: when the link is already terminal (done/failed),
		// reject a follow-up Refresh that lands inside the per-link
		// window. The latest job's created_at is the natural signal —
		// it advances on every successful Refresh. The 429 carries a
		// Retry-After hint so well-behaved clients can back off
		// without polling.
		if latest != nil && s.refreshCooldown > 0 {
			now := s.now
			if now == nil {
				now = time.Now
			}
			elapsed := now().Sub(latest.CreatedAt)
			if elapsed >= 0 && elapsed < s.refreshCooldown {
				retry := int((s.refreshCooldown - elapsed + time.Second - 1) / time.Second)
				if retry < 1 {
					retry = 1
				}
				// 带 slug：refresh 冷却是一种业务限流，客户端按 cooldown_active
				// 区分于全局 rate_limit_exceeded 才能给出针对性 UI 提示
				// （"x 秒后可重试"而非"请稍后重试"）。
				return httperr.NewWithCodeAndRetryAfter(http.StatusTooManyRequests, httperr.CodeCooldownActive, "refresh cooldown active", retry)
			}
		}

		resp, err = s.core.requeueExisting(lockCtx, current.ID, nil)
		return err
	})
	return resp, err
}
