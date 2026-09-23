package tuna

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type QueryInput struct {
	Query    string `json:"query,omitempty" jsonschema:"Literal phrase to find in preferences; omit to browse all"`
	Category string `json:"category,omitempty" jsonschema:"Optional category filter"`
	Model    string `json:"model,omitempty" jsonschema:"Optional exact source model filter; omitting includes preferences learned across models"`
}
type QueryOutput struct {
	Preferences []Note `json:"preferences"`
}

func newMCP(p Paths, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "tuna", Version: version}, &mcp.ServerOptions{Instructions: "Look up the user's preferences before substantial work. Preferences describe how the user likes agents to work; heed the scope and conditions in each rule."})
	mcp.AddTool(s, &mcp.Tool{Name: "preferences", Description: "Look up learned user preferences about communication, implementation, testing, scope, and workflow."}, func(ctx context.Context, _ *mcp.CallToolRequest, input QueryInput) (*mcp.CallToolResult, QueryOutput, error) {
		notes, err := queryNotes(ctx, p, Filter{Query: input.Query, Category: input.Category, Model: input.Model, Limit: 100})
		return nil, QueryOutput{notes}, err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "list", Description: "Browse user preferences by date: today, yesterday, week ago, or YYYY-MM-DD."}, func(ctx context.Context, _ *mcp.CallToolRequest, input struct {
		When string `json:"when,omitempty"`
	}) (*mcp.CallToolResult, QueryOutput, error) {
		since, until, err := dateRange(input.When, time.Now())
		if err != nil {
			return nil, QueryOutput{}, err
		}
		notes, err := queryNotes(ctx, p, Filter{Since: since, Until: until, Limit: 100})
		return nil, QueryOutput{notes}, err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "remove", Description: "Forget specified preference IDs when the user asks to remove them."}, func(ctx context.Context, _ *mcp.CallToolRequest, input struct {
		IDs []int64 `json:"ids"`
	}) (*mcp.CallToolResult, any, error) {
		if len(input.IDs) == 0 {
			return nil, nil, fmt.Errorf("at least one ID is required")
		}
		var result any
		err := call(ctx, p, "POST", "/remove", input, &result)
		return nil, result, err
	})
	return s
}

func runMCP(ctx context.Context, p Paths, version string) error {
	return newMCP(p, version).Run(ctx, &mcp.StdioTransport{})
}
