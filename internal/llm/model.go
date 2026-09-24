package llm

import einomodel "github.com/cloudwego/eino/components/model"

// Model is the product-facing model contract.
// It reuses the Eino agentic method set and does not add another wire protocol.
type Model = einomodel.AgenticModel
