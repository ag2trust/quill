package tools

import (
	"context"
	"encoding/json"
)

// search is the single dispatch point for the search tool. Task 5 replaces this file.
func (r *Registry) search(ctx context.Context, input json.RawMessage) Result {
	return errResult("search: not implemented")
}
