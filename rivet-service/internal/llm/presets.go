package llm

// Пресеты провайдеров (add-native-agent, design решение 11): готовые
// подключения для агента Rivet. Данные в коде, обновляются вместе с
// версией. Модели пресета сохраняются, только если discover провайдера
// недоступен (404) — иначе берётся список с провайдера. Fake-провайдера
// среди пресетов нет: он только для тестов.

// PresetModel — модель пресета с окном и ценами (USD за миллион токенов).
type PresetModel struct {
	ID              string  `json:"id"`
	ContextWindow   int64   `json:"context_window,omitempty"`
	MaxOutputTokens int64   `json:"max_output_tokens,omitempty"`
	InputPrice      float64 `json:"input_price,omitempty"`
	OutputPrice     float64 `json:"output_price,omitempty"`
}

// Preset — готовое подключение к провайдеру.
type Preset struct {
	ID      string        `json:"id"`
	Name    string        `json:"name"`
	Kind    string        `json:"kind"` // vendor | aggregator | local
	API     API           `json:"api"`
	BaseURL string        `json:"base_url"`
	NeedKey bool          `json:"need_key"`
	Models  []PresetModel `json:"models"`
	// Hint — где взять ключ или как запустить локальный сервер.
	Hint string `json:"hint,omitempty"`
}

// Presets — пресеты провайдеров в порядке показа в консоли. Models у пресета
// без своего списка — пустой срез, а не nil: в JSON это [] вместо null.
func Presets() []Preset {
	ps := presets()
	for i := range ps {
		if ps[i].Models == nil {
			ps[i].Models = []PresetModel{}
		}
	}
	return ps
}

func presets() []Preset {
	return []Preset{
		{ID: "deepseek", Name: "DeepSeek", Kind: "vendor", API: APIOpenAI, BaseURL: "https://api.deepseek.com", NeedKey: true,
			Models: []PresetModel{
				{ID: "deepseek-v4-flash", ContextWindow: 128000, MaxOutputTokens: 32768},
				{ID: "deepseek-v4-pro", ContextWindow: 128000, MaxOutputTokens: 32768},
			},
			Hint: "ключ в platform.deepseek.com → API keys"},
		{ID: "openai", Name: "OpenAI", Kind: "vendor", API: APIOpenAI, BaseURL: "https://api.openai.com/v1", NeedKey: true,
			Hint: "ключ в platform.openai.com → API keys; модели — с провайдера"},
		{ID: "anthropic", Name: "Anthropic", Kind: "vendor", API: APIAnthropic, BaseURL: "https://api.anthropic.com", NeedKey: true,
			Hint: "ключ в console.anthropic.com → API keys; модели — с провайдера"},
		{ID: "openrouter", Name: "OpenRouter", Kind: "aggregator", API: APIOpenAI, BaseURL: "https://openrouter.ai/api/v1", NeedKey: true,
			Hint: "ключ в openrouter.ai → Keys; модели — с провайдера"},
		{ID: "ollama", Name: "Ollama", Kind: "local", API: APIOpenAI, BaseURL: "http://localhost:11434/v1",
			Hint: "локальный сервер ollama serve, ключ не нужен"},
		{ID: "lmstudio", Name: "LM Studio", Kind: "local", API: APIOpenAI, BaseURL: "http://localhost:1234/v1",
			Hint: "локальный сервер LM Studio, ключ не нужен"},
	}
}

// PresetByID — пресет по идентификатору; ok=false — такого нет.
func PresetByID(id string) (Preset, bool) {
	for _, p := range Presets() {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}
