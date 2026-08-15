package worker

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"webtag/internal/service"
)

type readerInboxExpiryBatchProcessorStub struct {
	batches []int
}

func (s *readerInboxExpiryBatchProcessorStub) RunReaderInboxExpiryJob(_ context.Context, batchSize int) error {
	s.batches = append(s.batches, batchSize)
	return nil
}

func TestReaderInboxExpiryWorkerRunsOneInstallationBatch(t *testing.T) {
	processor := &readerInboxExpiryBatchProcessorStub{}
	worker := NewReaderInboxExpiryWorker(processor, 2*time.Minute)
	job := &river.Job[service.ReaderInboxExpiryJobArgs]{
		Args: service.ReaderInboxExpiryJobArgs{BatchSize: 37},
	}

	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("ReaderInboxExpiryWorker.Work() error = %v", err)
	}
	if len(processor.batches) != 1 || processor.batches[0] != 37 {
		t.Fatalf("batch sizes = %v, want one 37 batch", processor.batches)
	}
}

func TestReaderInboxExpiryWorkerDefaultsInvalidBatchSize(t *testing.T) {
	processor := &readerInboxExpiryBatchProcessorStub{}
	worker := NewReaderInboxExpiryWorker(processor, 0)
	job := &river.Job[service.ReaderInboxExpiryJobArgs]{}

	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("ReaderInboxExpiryWorker.Work() error = %v", err)
	}
	if processor.batches[0] != service.ReaderInboxExpiryBatchSize {
		t.Fatalf("default batch size = %d, want %d", processor.batches[0], service.ReaderInboxExpiryBatchSize)
	}
}

type readerInboxExpiryQueueProcessorStub struct{}

func (readerInboxExpiryQueueProcessorStub) RunReaderInboxSummaryJob(context.Context, service.ReaderInboxSummaryJobArgs, int, int) error {
	return nil
}

func (readerInboxExpiryQueueProcessorStub) RunReaderInboxExpiryJob(context.Context, int) error {
	return nil
}

func TestNewRiverQueueRegistersReaderInboxExpiryPeriodicJob(t *testing.T) {
	queue, err := NewRiverQueue(RiverQueueOptions{
		Pool:                 &pgxpool.Pool{},
		Processor:            &discardRecordingProcessor{},
		ReaderInboxProcessor: readerInboxExpiryQueueProcessorStub{},
	})
	if err != nil {
		t.Fatalf("NewRiverQueue() error = %v", err)
	}
	configValue := reflect.ValueOf(queue.client).Elem().FieldByName("config").Elem()
	periodicJobs := configValue.FieldByName("PeriodicJobs")
	if !periodicJobs.IsValid() || periodicJobs.Len() != 1 {
		t.Fatalf("River periodic jobs = %v, want one Reader expiry schedule", periodicJobs)
	}
}
