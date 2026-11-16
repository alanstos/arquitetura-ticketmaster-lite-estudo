package search

// Analyzers: como o texto é quebrado em termos, tanto ao indexar quanto ao buscar.
//
//	"Casarão Cultural" --pt_text-->   [casara, cultural]            (radical + sem acento)
//	                   --pt_prefix--> [ca, cas, casa, casar, ..., casarao, cu, cul, ...]
//
// pt_text: minúsculas + stemmer leve de português + remove acentos. "casas" e
// "casa" viram o mesmo termo; "São Paulo" casa com "sao paulo". O stemmer roda
// antes do asciifolding, então corta o "ão" de "casarão" (casara). Não há
// problema: o mesmo analyzer é usado na indexação e na busca.
//
// pt_prefix (só na indexação): gera todos os prefixos (edge n-grams) de cada
// palavra. Na busca usa pt_plain (sem n-gram): a palavra digitada é comparada
// com os prefixos guardados, então "casa" acha "casarão" e "casas".
//
// A fuzziness da query (query.go) cobre erro de digitação ("qest" ~ "quest").
// Fuzzy sobre o campo de prefixos também cobre "casa" ~ "casi" (prefixo de
// "casinha"), a 1 letra de distância.
var indexSettings = map[string]any{
	"number_of_shards":   1,
	"number_of_replicas": 0, // nó único (estudo); em produção, >= 1
	"analysis": map[string]any{
		"filter": map[string]any{
			"pt_stemmer": map[string]any{"type": "stemmer", "language": "light_portuguese"},
			"prefixes":   map[string]any{"type": "edge_ngram", "min_gram": 2, "max_gram": 20},
		},
		"analyzer": map[string]any{
			"pt_text": map[string]any{
				"tokenizer": "standard",
				"filter":    []string{"lowercase", "pt_stemmer", "asciifolding"},
			},
			"pt_plain": map[string]any{
				"tokenizer": "standard",
				"filter":    []string{"lowercase", "asciifolding"},
			},
			"pt_prefix": map[string]any{
				"tokenizer": "standard",
				"filter":    []string{"lowercase", "asciifolding", "prefixes"},
			},
		},
	},
}

// textField: campo de texto que também é copiado para "all" (usado para exigir
// todas as palavras da busca, mesmo espalhadas entre nome, local e cidade).
func textField() map[string]any {
	return map[string]any{"type": "text", "analyzer": "pt_text", "copy_to": "all"}
}

var indexMappings = map[string]any{
	"dynamic": "strict", // campo desconhecido no documento = erro (pega bug cedo)
	"properties": map[string]any{
		"name":        textField(),
		"description": textField(),
		"venue_name":  textField(),
		"city":        textField(),
		"starts_at":   map[string]any{"type": "date"},
		"all": map[string]any{
			"type":     "text",
			"analyzer": "pt_text",
			"fields": map[string]any{
				"prefix": map[string]any{
					"type":            "text",
					"analyzer":        "pt_prefix",
					"search_analyzer": "pt_plain",
				},
			},
		},
	},
}
