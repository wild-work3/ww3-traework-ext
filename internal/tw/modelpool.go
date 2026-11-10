package tw

import "sync"

// modelFunc 记录“模型 → function”的映射，仅由取模型列表时**只增不改**地填充。
var (
	modelFuncMu sync.RWMutex
	modelFunc   = map[string]string{
		"qwen3.8-flash": FunctionCode,
		"glm-5.3-flash": FunctionCode,
	}
)

// RecordModelFunc 把一个模型的 function 记入池；已存在则**不覆盖**。
//
// 只增不改：同一模型若在两个变体下 function 不同，先到者胜，避免同一进程内
// 反复改写导致请求体内 function 抖动（上游会报 4001 param is invalid）。
func RecordModelFunc(model, function string) {
	if model == "" || function == "" {
		return
	}
	modelFuncMu.Lock()
	defer modelFuncMu.Unlock()
	if _, ok := modelFunc[model]; ok {
		return
	}
	modelFunc[model] = function
}

// FunctionForModel 返回模型已登记的 function；未登记返回空串。
func FunctionForModel(model string) string {
	modelFuncMu.RLock()
	defer modelFuncMu.RUnlock()
	return modelFunc[model]
}

// OtherFunction 返回同一插件另一变体的 function。
func OtherFunction(function string) string {
	if function == Function {
		return FunctionCode
	}
	return Function
}
