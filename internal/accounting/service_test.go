package accounting

import (
	"context"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

type memoryRepository struct {
	usage []domain.DailyUsage
	rates []domain.BillingRate
}

func (repository *memoryRepository) ListDailyUsage(context.Context, string, string, string) ([]domain.DailyUsage, error) {
	return repository.usage, nil
}
func (repository *memoryRepository) PutBillingRate(_ context.Context, rate domain.BillingRate, _ domain.AuditEvent) error {
	repository.rates = append(repository.rates, rate)
	return nil
}
func (repository *memoryRepository) ListBillingRates(context.Context) ([]domain.BillingRate, error) {
	return repository.rates, nil
}

func TestSummaryUsesImmutableEffectiveRate(t *testing.T) {
	repository := &memoryRepository{
		usage: []domain.DailyUsage{{
			Date: "2026-07-29", OwnerID: "user-1", CPUUsageMicros: 3_600_000_000,
			MemoryByteSeconds: (1 << 30) * 3600, GPUDeviceSeconds: 3600,
			WorkspaceByteSeconds: (1 << 30) * 86400,
		}},
		rates: []domain.BillingRate{{
			Currency: "CNY", EffectiveAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			CPUCoreHourMicros: 1_000_000, MemoryGiBHourMicros: 2_000_000,
			GPUDeviceHourMicros: 3_000_000, StorageGiBDayMicros: 4_000_000,
		}},
	}
	summary, err := New(repository).Summary(context.Background(), "2026-07-29", "2026-07-29", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if summary.Cost.TotalCostMicros != 10_000_000 {
		t.Fatalf("total cost = %d, want 10000000", summary.Cost.TotalCostMicros)
	}
}
