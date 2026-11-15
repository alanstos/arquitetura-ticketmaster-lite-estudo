package search

import "ticketmaster-lite/backend/internal/store"

// buildQuery monta a busca:
//
//	filter (não afeta o score): só eventos futuros + intervalo de datas.
//	must:   pelo menos UM dos jeitos de casar, exigindo TODAS as palavras digitadas:
//	          - all        palavra inteira/radical, com fuzziness ("qest" ~ "quest")
//	          - all.prefix prefixo, com fuzziness ("casa" ~ "casarão", "casinha")
//	should: reforço de score quando a palavra aparece no nome (^3) ou no local/cidade (^2).
//
// fuzziness AUTO: 0 edições para termos de 1-2 letras, 1 para 3-5, 2 para 6+.
// prefix_length 1: a primeira letra precisa estar certa. Isso limita a expansão
// de termos (desempenho) e corta ruído como "casa" ~ "rasa".
func buildQuery(f store.SearchFilter) map[string]any {
	filters := []any{
		map[string]any{"range": map[string]any{"starts_at": map[string]any{"gte": "now"}}},
	}
	// Datas no fuso de São Paulo, inclusivas: "||/d" arredonda para o início
	// (gte) ou para o fim (lte) do dia, no time_zone informado.
	dateRange := map[string]any{"time_zone": "America/Sao_Paulo"}
	if f.StartDate != nil {
		dateRange["gte"] = f.StartDate.Format("2006-01-02") + "||/d"
	}
	if f.EndDate != nil {
		dateRange["lte"] = f.EndDate.Format("2006-01-02") + "||/d"
	}
	if len(dateRange) > 1 {
		filters = append(filters, map[string]any{"range": map[string]any{"starts_at": dateRange}})
	}

	fuzzyMatch := func(field string, boost float64) map[string]any {
		return map[string]any{"match": map[string]any{field: map[string]any{
			"query":         f.Keyword,
			"operator":      "and",
			"fuzziness":     "AUTO",
			"prefix_length": 1,
			"boost":         boost,
		}}}
	}

	return map[string]any{
		"size":    maxResults,
		"_source": false, // só precisamos do _id; os dados vêm do Postgres
		"query": map[string]any{"bool": map[string]any{
			"filter": filters,
			"must": map[string]any{"bool": map[string]any{
				"should": []any{
					fuzzyMatch("all", 2),
					fuzzyMatch("all.prefix", 1),
				},
				"minimum_should_match": 1,
			}},
			"should": []any{
				map[string]any{"multi_match": map[string]any{
					"query":     f.Keyword,
					"fields":    []string{"name^3", "venue_name^2", "city^2", "description"},
					"fuzziness": "AUTO",
				}},
			},
		}},
		"sort": []any{"_score", map[string]any{"starts_at": "asc"}},
	}
}
