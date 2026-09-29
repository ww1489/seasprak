package llm

import (
	"encoding/json"
	"reflect"
	"sort"

	"github.com/cloudwego/eino/schema"
)

// agenticopenai v0.2.4 responses_content_block_extra.go stores item identity
// under this private key, as either blockExtraItemID (a named string) or string.
// Keep this sole compatibility dependency here; real-HTTP contract tests cover
// identity, Eino block indices and the adapter's subsequent input conversion.
const responsesItemIDKey = "openai-item-id"

type responsesCollectorKey struct{}

type responsesReasoningItem struct {
	outputIndex int
	signature   string
	blockIndex  int
	seen        bool
	final       bool
}

// Owned by the existing per-response UsageCollector and protected by its mutex.
// Only reasoning identity and opaque replay data are retained, never a response
// body. Total retained data is bounded separately from the single-frame buffer.
// None of this state is part of UsageSnapshot, diagnostics or message Extra.
type responsesReasoningCollection struct {
	items    map[string]*responsesReasoningItem
	outputs  map[int]string
	blocks   map[int]string
	bytes    int
	invalid  bool
	terminal bool
}

func newResponsesCollector(limit int) *UsageCollector {
	c := NewUsageCollector("openai-responses", limit)
	c.responses = &responsesReasoningCollection{
		items: make(map[string]*responsesReasoningItem), outputs: make(map[int]string), blocks: make(map[int]string),
	}
	return c
}

type responsesWireReasoning struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Encrypted string `json:"encrypted_content"`
}

func (c *UsageCollector) collectResponsesReasoning(data []byte) {
	r := c.responses
	if r.invalid {
		return
	}
	var event struct {
		Type        string                 `json:"type"`
		OutputIndex *int                   `json:"output_index"`
		Item        responsesWireReasoning `json:"item"`
		Response    struct {
			Output []responsesWireReasoning `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &event) != nil || r.terminal {
		r.invalid = true
		return
	}
	switch event.Type {
	case "response.output_item.added", "response.output_item.done":
		if event.Item.Type == "reasoning" {
			if event.OutputIndex == nil {
				r.invalid = true
				return
			}
			c.recordResponsesReasoning(event.Item, *event.OutputIndex, false)
		} else if event.OutputIndex != nil && r.outputs[*event.OutputIndex] != "" {
			r.invalid = true
		}
	case "response.completed", "response.incomplete", "response.failed":
		// output_index is defined by the terminal output array, not arrival order.
		for index, item := range event.Response.Output {
			if item.Type == "reasoning" {
				c.recordResponsesReasoning(item, index, true)
			} else if r.outputs[index] != "" {
				r.invalid = true
			}
		}
		r.terminal = true
	}
}

func (c *UsageCollector) recordResponsesReasoning(w responsesWireReasoning, index int, final bool) {
	r := c.responses
	if r.invalid {
		return
	}
	if w.ID == "" || index < 0 || (r.outputs[index] != "" && r.outputs[index] != w.ID) {
		r.invalid = true
		return
	}
	item := r.items[w.ID]
	if item == nil {
		// Account for map/entry overhead as well as retained string bytes. This
		// also bounds the number of items when the provider sends empty fields.
		size := len(w.ID) + 128
		if size > c.limit-r.bytes {
			r.invalid = true
			return
		}
		r.bytes += size
		item = &responsesReasoningItem{outputIndex: index}
		r.items[w.ID], r.outputs[index] = item, w.ID
	}
	if item.outputIndex != index || (final && item.final) {
		r.invalid = true
		return
	}
	item.final = item.final || final
	if w.Encrypted != "" {
		if item.signature != "" && item.signature != w.Encrypted {
			r.invalid = true
			return
		}
		if item.signature == "" {
			if len(w.Encrypted) > c.limit-r.bytes {
				r.invalid = true
				return
			}
			r.bytes += len(w.Encrypted)
			item.signature = w.Encrypted
		}
	}
}

func responsesBlockItemID(block *schema.ContentBlock) string {
	value := reflect.ValueOf(block.Extra[responsesItemIDKey])
	if value.IsValid() && value.Kind() == reflect.String {
		return value.String()
	}
	return ""
}

// Strip upstream signature chunks before Eino concatenation. The same opaque
// value can appear in added/done/completed, but is not a concatenatable delta.
// Associate by the actual provider ID; never assume output_index equals Eino's
// StreamingMeta.Index, or infer identity from the order of emitted blocks.
func (c *UsageCollector) associateResponsesReasoning(msg *schema.AgenticMessage) (*schema.AgenticMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.responses
	if c.disabled || r.invalid {
		return nil, invalid("invalid Responses reasoning replay metadata")
	}
	copyMsg := *msg
	copyMsg.ContentBlocks = append([]*schema.ContentBlock(nil), msg.ContentBlocks...)
	for i, block := range msg.ContentBlocks {
		if block == nil || block.Reasoning == nil {
			continue
		}
		id := responsesBlockItemID(block)
		item := r.items[id]
		if item == nil || block.StreamingMeta == nil || block.StreamingMeta.Index < 0 {
			return nil, invalid("missing Responses reasoning item association")
		}
		index := block.StreamingMeta.Index
		if (item.seen && item.blockIndex != index) || (r.blocks[index] != "" && r.blocks[index] != id) {
			return nil, invalid("conflicting Responses reasoning item association")
		}
		if block.Reasoning.Signature != "" && block.Reasoning.Signature != item.signature {
			return nil, invalid("conflicting Responses reasoning replay metadata")
		}
		item.seen, item.blockIndex, r.blocks[index] = true, index, id
		copyBlock, copyReasoning := *block, *block.Reasoning
		copyReasoning.Signature = ""
		copyBlock.Reasoning = &copyReasoning
		copyMsg.ContentBlocks[i] = &copyBlock
	}
	return &copyMsg, nil
}

// Called only after the SDK stream ends and the product validates its terminal
// status. Preserve the upstream blocks/Extra for replay; add only one signature
// chunk per proven item at its exact Eino block index. Missing encrypted_content
// remains missing; missing identity or incomplete capture is always an error.
func (c *UsageCollector) responsesReasoningPatches() ([]*schema.ContentBlock, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.responses
	if c.disabled || r.invalid || !r.terminal || !c.snapshot.Complete {
		return nil, invalid("incomplete Responses reasoning replay metadata")
	}
	var patches []*schema.ContentBlock
	for _, item := range r.items {
		if !item.seen || !item.final {
			return nil, invalid("missing Responses reasoning item association")
		}
		if item.signature != "" {
			patches = append(patches, schema.NewContentBlockChunk(&schema.Reasoning{Signature: item.signature}, &schema.StreamingMeta{Index: item.blockIndex}))
		}
	}
	sort.Slice(patches, func(i, j int) bool { return patches[i].StreamingMeta.Index < patches[j].StreamingMeta.Index })
	return patches, nil
}
