package orchestrator

import (
	"math"

	"github.com/PavluninVladimir/rivet/internal/analyst"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/llm"
)

// opsPriceSnapshot копирует только тариф конкретного выбранного connection/model.
// Каталог после вызова может измениться или быть удалён и не является историей цен.
func opsPriceSnapshot(models []domain.ModelEntry, model string) *domain.ModelEntry {
	var selected *domain.ModelEntry
	copyPrice := func(p *int64) *int64 {
		if p == nil {
			return nil
		}
		n := *p
		return &n
	}
	for _, m := range models {
		if m.ID != model {
			continue
		}
		if selected != nil {
			return nil // неоднозначный повреждённый каталог
		}
		selected = &domain.ModelEntry{ID: model, InputPrice: copyPrice(m.InputPrice),
			OutputPrice: copyPrice(m.OutputPrice), CachedInputPrice: copyPrice(m.CachedInputPrice)}
	}
	return selected
}

// opsCost — справочная стоимость по тарифу, захваченному до вызова. Неполные
// счётчики, неизвестная цена кеша/попытки остаются nil, а не бесплатным вызовом.
func opsCost(use llm.Usage, price *domain.ModelEntry) *float64 {
	if price == nil || use.CostIncomplete || use.InputTokens == nil || use.OutputTokens == nil ||
		price.InputPrice == nil || price.OutputPrice == nil || *use.InputTokens < 0 || *use.OutputTokens < 0 ||
		*price.InputPrice < 0 || *price.OutputPrice < 0 {
		return nil
	}
	if use.CacheWriteTokens != nil && *use.CacheWriteTokens != 0 {
		return nil // отдельного тарифа записи кеша в каталоге нет
	}
	if use.CachedInputTokens != nil && (*use.CachedInputTokens < 0 || *use.CachedInputTokens > *use.InputTokens) {
		return nil
	}
	var cost *float64
	if price.CachedInputPrice != nil {
		if *price.CachedInputPrice < 0 {
			return nil
		}
		if *price.CachedInputPrice != *price.InputPrice {
			if use.CachedInputTokens == nil {
				return nil
			}
			fresh, zero := *use.InputTokens-*use.CachedInputTokens, int64(0)
			cost = analyst.Cost(llm.Usage{InputTokens: &fresh, OutputTokens: use.OutputTokens}, price.InputPrice, price.OutputPrice)
			cached := analyst.Cost(llm.Usage{InputTokens: use.CachedInputTokens, OutputTokens: &zero}, price.CachedInputPrice, &zero)
			*cost += *cached
		}
	}
	if cost == nil {
		cost = analyst.Cost(use, price.InputPrice, price.OutputPrice)
	}
	// usage_records.cost_usd — numeric(12,6). Не терять известные токены
	// из-за переполнения денежного столбца при недостоверных provider values.
	if math.IsNaN(*cost) || math.IsInf(*cost, 0) || *cost < 0 || *cost > 999999.999999 {
		return nil
	}
	return cost
}
