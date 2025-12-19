package decision

import (
	"context"

	"brale/internal/gateway/provider"
)

// PromptBuilder generates system/user prompts and optional images from a decision Context.
// insights carries optional multi-agent outputs for inclusion in the prompt.
type PromptBuilder interface {
	Build(ctx context.Context, input Context, insights []AgentInsight) (system string, user string, images []provider.ImagePayload, err error)
}
