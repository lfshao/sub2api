package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

const defaultClaudeCLIToolUsePrefixWait = 200 * time.Millisecond

type claudeCLIOutputCollector struct {
	mu               sync.Mutex
	data             bytes.Buffer
	notify           chan struct{}
	blocksByIndex    map[int]map[string]any
	inputJSONByIndex map[int]string
	prefixByToolUse  map[string][]map[string]any
}

func newClaudeCLIOutputCollector() *claudeCLIOutputCollector {
	return &claudeCLIOutputCollector{
		notify:          make(chan struct{}),
		prefixByToolUse: make(map[string][]map[string]any),
	}
}

func (c *claudeCLIOutputCollector) collectFrom(r io.Reader) error {
	if c == nil {
		_, err := io.Copy(io.Discard, r)
		return err
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), claudeCLIStreamMaxLineSize)
	for scanner.Scan() {
		line := scanner.Bytes()
		c.appendLine(line)
		c.observeLine(line)
	}
	return scanner.Err()
}

func (c *claudeCLIOutputCollector) Bytes() []byte {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.data.Bytes()...)
}

func (c *claudeCLIOutputCollector) WaitToolUsePrefix(ctx context.Context, toolUseID string, timeout time.Duration) []map[string]any {
	if c == nil || toolUseID == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = defaultClaudeCLIToolUsePrefixWait
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		c.mu.Lock()
		if prefix, ok := c.prefixByToolUse[toolUseID]; ok {
			out := cloneClaudeCLIBlockSlice(prefix)
			c.mu.Unlock()
			return out
		}
		notify := c.notify
		c.mu.Unlock()

		select {
		case <-notify:
		case <-waitCtx.Done():
			return nil
		}
	}
}

func (c *claudeCLIOutputCollector) appendLine(line []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = c.data.Write(line)
	_ = c.data.WriteByte('\n')
}

func (c *claudeCLIOutputCollector) observeLine(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	var envelope claudeCLIStreamEnvelope
	if err := json.Unmarshal(trimmed, &envelope); err != nil || envelope.Type != "stream_event" {
		return
	}
	var event claudeCLIStreamEvent
	if err := json.Unmarshal(envelope.Event, &event); err != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	switch event.Type {
	case "message_start":
		c.blocksByIndex = make(map[int]map[string]any)
		c.inputJSONByIndex = make(map[int]string)
	case "content_block_start":
		if len(event.ContentBlock) == 0 {
			return
		}
		block := cloneClaudeCLIBlock(event.ContentBlock)
		c.blocksByIndex[event.Index] = block
		if blockType, _ := block["type"].(string); blockType == "tool_use" {
			if id, _ := block["id"].(string); id != "" {
				c.prefixByToolUse[id] = c.prefixBlocksLocked(event.Index)
				c.broadcastLocked()
			}
		}
	case "content_block_delta":
		block := c.blocksByIndex[event.Index]
		if block == nil {
			return
		}
		switch event.Delta.Type {
		case "thinking_delta":
			thinking, _ := block["thinking"].(string)
			block["thinking"] = thinking + event.Delta.Thinking
		case "signature_delta":
			signature, _ := block["signature"].(string)
			block["signature"] = signature + event.Delta.Signature
		case "text_delta":
			text, _ := block["text"].(string)
			block["text"] = text + event.Delta.Text
		case "input_json_delta":
			c.inputJSONByIndex[event.Index] += event.Delta.PartialJSON
		}
	case "content_block_stop":
		inputJSON := c.inputJSONByIndex[event.Index]
		if inputJSON == "" {
			return
		}
		var input map[string]any
		if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
			return
		}
		if block := c.blocksByIndex[event.Index]; block != nil {
			block["input"] = input
		}
	}
}

func (c *claudeCLIOutputCollector) prefixBlocksLocked(toolIndex int) []map[string]any {
	indices := make([]int, 0, len(c.blocksByIndex))
	for index := range c.blocksByIndex {
		if index < toolIndex {
			indices = append(indices, index)
		}
	}
	sort.Ints(indices)
	out := make([]map[string]any, 0, len(indices))
	for _, index := range indices {
		block := c.blocksByIndex[index]
		blockType, _ := block["type"].(string)
		if blockType == "tool_use" {
			continue
		}
		out = append(out, cloneClaudeCLIBlock(block))
	}
	return out
}

func (c *claudeCLIOutputCollector) broadcastLocked() {
	close(c.notify)
	c.notify = make(chan struct{})
}

func cloneClaudeCLIBlockSlice(blocks []map[string]any) []map[string]any {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		out = append(out, cloneClaudeCLIBlock(block))
	}
	return out
}

func cloneClaudeCLIBlock(block map[string]any) map[string]any {
	if block == nil {
		return nil
	}
	out := make(map[string]any, len(block))
	for key, value := range block {
		out[key] = value
	}
	return out
}

func claudeCLIToolUsePrefixBlocks(ctx context.Context, collector *claudeCLIOutputCollector, calls []claudeCLIToolCall) []map[string]any {
	if collector == nil || len(calls) == 0 {
		return nil
	}
	prefix := collector.WaitToolUsePrefix(ctx, calls[0].ID, defaultClaudeCLIToolUsePrefixWait)
	if len(prefix) == 0 {
		return nil
	}
	return prefix
}

func validateClaudeCLIBlockForSyntheticResponse(block map[string]any) error {
	if block == nil {
		return fmt.Errorf("claude cli synthetic response: content block is nil")
	}
	blockType, _ := block["type"].(string)
	if blockType == "" {
		return fmt.Errorf("claude cli synthetic response: content block type is empty")
	}
	return nil
}
