// Package accounting turns cumulative Agent usage counters into reproducible
// daily usage and cost summaries.
package accounting

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

var ErrInvalid = errors.New("invalid accounting request")

type Repository interface {
	ListDailyUsage(context.Context, string, string, string) ([]domain.DailyUsage, error)
	PutBillingRate(context.Context, domain.BillingRate, domain.AuditEvent) error
	ListBillingRates(context.Context) ([]domain.BillingRate, error)
}

type Service struct {
	repository Repository
	now        func() time.Time
}

func New(repository Repository) *Service {
	return &Service{repository: repository, now: time.Now}
}

func (service *Service) Summary(ctx context.Context, from, to, ownerID string) (domain.UsageSummary, error) {
	if _, _, err := validateRange(from, to); err != nil {
		return domain.UsageSummary{}, err
	}
	usage, err := service.repository.ListDailyUsage(ctx, from, to, strings.TrimSpace(ownerID))
	if err != nil {
		return domain.UsageSummary{}, err
	}
	rates, err := service.repository.ListBillingRates(ctx)
	if err != nil {
		return domain.UsageSummary{}, err
	}
	return summarize(from, to, strings.TrimSpace(ownerID), usage, rates), nil
}

func (service *Service) UserSummaries(ctx context.Context, from, to string) ([]domain.UsageSummary, error) {
	if _, _, err := validateRange(from, to); err != nil {
		return nil, err
	}
	usage, err := service.repository.ListDailyUsage(ctx, from, to, "")
	if err != nil {
		return nil, err
	}
	rates, err := service.repository.ListBillingRates(ctx)
	if err != nil {
		return nil, err
	}
	grouped := map[string][]domain.DailyUsage{}
	for _, item := range usage {
		grouped[item.OwnerID] = append(grouped[item.OwnerID], item)
	}
	summaries := make([]domain.UsageSummary, 0, len(grouped))
	for ownerID, items := range grouped {
		summaries = append(summaries, summarize(from, to, ownerID, items, rates))
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Cost.TotalCostMicros != summaries[j].Cost.TotalCostMicros {
			return summaries[i].Cost.TotalCostMicros > summaries[j].Cost.TotalCostMicros
		}
		return summaries[i].OwnerID < summaries[j].OwnerID
	})
	return summaries, nil
}

func (service *Service) Rates(ctx context.Context) ([]domain.BillingRate, error) {
	return service.repository.ListBillingRates(ctx)
}

func (service *Service) CreateRate(ctx context.Context, rate domain.BillingRate, actor domain.Principal) (domain.BillingRate, error) {
	rate.Currency = strings.ToUpper(strings.TrimSpace(rate.Currency))
	if len(rate.Currency) != 3 || rate.CPUCoreHourMicros < 0 || rate.MemoryGiBHourMicros < 0 ||
		rate.GPUDeviceHourMicros < 0 || rate.StorageGiBDayMicros < 0 {
		return domain.BillingRate{}, ErrInvalid
	}
	now := service.now().UTC()
	if rate.EffectiveAt.IsZero() {
		rate.EffectiveAt = now
	} else {
		rate.EffectiveAt = rate.EffectiveAt.UTC()
	}
	// Backdating a new tariff would silently rewrite already presented costs.
	if rate.EffectiveAt.Before(now.Add(-time.Minute)) || rate.EffectiveAt.After(now.Add(366*24*time.Hour)) {
		return domain.BillingRate{}, ErrInvalid
	}
	rates, err := service.repository.ListBillingRates(ctx)
	if err != nil {
		return domain.BillingRate{}, err
	}
	if len(rates) > 0 && rate.EffectiveAt.Before(rates[len(rates)-1].EffectiveAt) {
		return domain.BillingRate{}, ErrInvalid
	}
	rate.ID = "rate_" + randomID()
	rate.CreatedAt, rate.CreatedBy = now, actor.Subject
	audit := domain.AuditEvent{
		ID: "billing_rate_created_" + rate.ID, ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "billing_rate.created", TargetKind: "billing_rate", TargetID: rate.ID,
		Outcome: "succeeded", OccurredAt: now,
	}
	if err := service.repository.PutBillingRate(ctx, rate, audit); err != nil {
		return domain.BillingRate{}, err
	}
	return rate, nil
}

func summarize(from, to, ownerID string, usage []domain.DailyUsage, rates []domain.BillingRate) domain.UsageSummary {
	result := domain.UsageSummary{OwnerID: ownerID, From: from, To: to, Currency: "CNY", Daily: []domain.DailyUsageCost{}}
	for _, item := range usage {
		if result.OwnerName == "" {
			result.OwnerName = item.OwnerName
		}
		rate := rateForDate(item.Date, rates)
		if rate.Currency != "" {
			result.Currency = rate.Currency
		}
		cost := calculateCost(item, rate)
		result.Daily = append(result.Daily, domain.DailyUsageCost{DailyUsage: item, Cost: cost})
		result.CPUUsageMicros += item.CPUUsageMicros
		result.MemoryByteSeconds += item.MemoryByteSeconds
		result.GPUDeviceSeconds += item.GPUDeviceSeconds
		result.WallTimeMillis += item.WallTimeMillis
		result.WorkspaceByteSeconds += item.WorkspaceByteSeconds
		if item.PeakWorkspaceBytes > result.PeakWorkspaceBytes {
			result.PeakWorkspaceBytes = item.PeakWorkspaceBytes
		}
		result.Cost.CPUCostMicros += cost.CPUCostMicros
		result.Cost.MemoryCostMicros += cost.MemoryCostMicros
		result.Cost.GPUCostMicros += cost.GPUCostMicros
		result.Cost.StorageCostMicros += cost.StorageCostMicros
		result.Cost.TotalCostMicros += cost.TotalCostMicros
	}
	return result
}

func rateForDate(date string, rates []domain.BillingRate) domain.BillingRate {
	at, _ := time.Parse("2006-01-02", date)
	at = at.Add(24*time.Hour - time.Nanosecond)
	var selected domain.BillingRate
	for _, rate := range rates {
		if rate.EffectiveAt.After(at) {
			break
		}
		selected = rate
	}
	return selected
}

func calculateCost(usage domain.DailyUsage, rate domain.BillingRate) domain.UsageCost {
	cost := domain.UsageCost{
		CPUCostMicros:     multiplyRatio(usage.CPUUsageMicros, rate.CPUCoreHourMicros, 1_000_000*3600),
		MemoryCostMicros:  multiplyRatio(usage.MemoryByteSeconds, rate.MemoryGiBHourMicros, (1<<30)*3600),
		GPUCostMicros:     multiplyRatio(usage.GPUDeviceSeconds, rate.GPUDeviceHourMicros, 3600),
		StorageCostMicros: multiplyRatio(usage.WorkspaceByteSeconds, rate.StorageGiBDayMicros, (1<<30)*86400),
	}
	cost.TotalCostMicros = cost.CPUCostMicros + cost.MemoryCostMicros + cost.GPUCostMicros + cost.StorageCostMicros
	return cost
}

func multiplyRatio(quantity uint64, price int64, denominator int64) int64 {
	if quantity == 0 || price <= 0 || denominator <= 0 {
		return 0
	}
	numerator := new(big.Int).Mul(new(big.Int).SetUint64(quantity), big.NewInt(price))
	numerator.Add(numerator, big.NewInt(denominator/2))
	numerator.Quo(numerator, big.NewInt(denominator))
	if !numerator.IsInt64() {
		return int64(^uint64(0) >> 1)
	}
	return numerator.Int64()
}

func validateRange(from, to string) (time.Time, time.Time, error) {
	start, startErr := time.Parse("2006-01-02", from)
	end, endErr := time.Parse("2006-01-02", to)
	if startErr != nil || endErr != nil || end.Before(start) || end.Sub(start) > 366*24*time.Hour {
		return time.Time{}, time.Time{}, ErrInvalid
	}
	return start, end, nil
}

func randomID() string {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(value)
}
