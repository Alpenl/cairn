package service

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

// JobReadService 负责 /api/jobs/{id} 的读取：返回任务状态以及任务对应链接
// 完成时的最终响应快照。
type JobReadService struct {
	jobs  repository.JobStore
	links linkDetailByIDReader
}

type linkDetailByIDReader interface {
	GetDetailByID(context.Context, uuid.UUID) (*repository.LinkDetailProjection, error)
}

// NewJobReadService accepts the exact detail lookup this consumer uses.
func NewJobReadService(jobs repository.JobStore, links linkDetailByIDReader) *JobReadService {
	return &JobReadService{
		jobs:  jobs,
		links: links,
	}
}

// Get 查询任务详情；任务已完成时顺带加载对应链接，方便客户端轮询到完成时
// 一次拿全结果而非再发一次 /api/links/{id}。
func (s *JobReadService) Get(ctx context.Context, jobID string) (dto.JobResponse, error) {
	id, err := uuid.Parse(strings.TrimSpace(jobID))
	if err != nil {
		return dto.JobResponse{}, httperr.NewWithCode(http.StatusBadRequest, httperr.CodeInvalidJobID, "invalid job id")
	}

	job, err := s.jobs.GetByID(ctx, id)
	if err != nil {
		return dto.JobResponse{}, err
	}
	if job == nil {
		return dto.JobResponse{}, httperr.NewWithCode(http.StatusNotFound, httperr.CodeJobNotFound, "job not found")
	}

	resp := dto.JobResponse{
		ID:            job.ID.String(),
		LinkID:        job.LinkID.String(),
		Status:        string(job.Status),
		ErrorCategory: stringPtr(deriveJobErrorCategory(*job)),
		ErrorMsg:      job.ErrorMsg,
	}

	if job.Status == model.JobStatusDone {
		link, err := s.links.GetDetailByID(ctx, job.LinkID)
		if err != nil {
			return dto.JobResponse{}, err
		}
		if link != nil {
			mapped := linkDetailToResponse(*link)
			resp.Link = &mapped
		}
	}

	return resp, nil
}

// List returns a batch of parse-job snapshots so the UI can poll many active
// jobs with one request instead of one request per job.
func (s *JobReadService) List(ctx context.Context, jobIDs []string) ([]dto.JobResponse, error) {
	if len(jobIDs) == 0 {
		return []dto.JobResponse{}, nil
	}

	parsed := make([]uuid.UUID, 0, len(jobIDs))
	indexByID := make(map[uuid.UUID]int, len(jobIDs))
	for _, raw := range jobIDs {
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			return nil, httperr.NewWithCode(http.StatusBadRequest, httperr.CodeInvalidJobID, "invalid job id")
		}
		if _, seen := indexByID[id]; seen {
			continue
		}
		indexByID[id] = len(parsed)
		parsed = append(parsed, id)
	}

	jobs, err := s.jobs.ListByIDs(ctx, parsed)
	if err != nil {
		return nil, err
	}

	out := make([]dto.JobResponse, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, dto.JobResponse{
			ID:            job.ID.String(),
			LinkID:        job.LinkID.String(),
			Status:        string(job.Status),
			ErrorCategory: stringPtr(deriveJobErrorCategory(job)),
			ErrorMsg:      job.ErrorMsg,
		})
	}

	slices.SortFunc(out, func(a, b dto.JobResponse) int {
		return indexByID[uuid.MustParse(a.ID)] - indexByID[uuid.MustParse(b.ID)]
	})

	return out, nil
}
