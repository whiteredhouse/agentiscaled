package caller

import (
	"context"
	"errors"
	"io"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ProxyTools adds to server every tool of the server that remote is
// connected to, each calling that tool through remote. describe, if set,
// may change a tool before it is added, such as its description; its input
// schema should stay as remote serves it.
func ProxyTools(ctx context.Context, server *mcp.Server, remote *mcp.ClientSession, describe func(*mcp.Tool)) error {
	var cursor string
	for {
		page, err := remote.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return err
		}
		for _, tool := range page.Tools {
			if describe != nil {
				describe(tool)
			}
			name := tool.Name
			server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return remote.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: req.Params.Arguments})
			})
		}
		if cursor = page.NextCursor; cursor == "" {
			return nil
		}
	}
}

// ServeUntilClosed runs server on client until the client closes its end,
// which is the normal way to stop and no error.
func ServeUntilClosed(ctx context.Context, server *mcp.Server, client mcp.Transport) error {
	if err := server.Run(ctx, client); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, mcp.ErrConnectionClosed) && ctx.Err() == nil {
		return err
	}
	return nil
}
