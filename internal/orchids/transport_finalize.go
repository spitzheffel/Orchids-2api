package orchids

import (
	"context"
	"fmt"
	"sync"

	"orchids-api/internal/upstream"
)

func orchidsFinishReason(state *requestState) string {
	if state.sawToolCall {
		return "tool-calls"
	}
	return "stop"
}

func finalizeOrchidsTransport(
	ctx context.Context,
	transport string,
	state *requestState,
	onMessage func(upstream.SSEMessage),
	fsWG *sync.WaitGroup,
) error {
	if state.errorMsg != "" {
		return fmt.Errorf("orchids upstream error: %s", state.errorMsg)
	}

	if !state.finishSent {
		state.finishReason = orchidsFinishReason(state)
		emitOrchidsCompletionTail(state, onMessage)
	}

	return nil
}
