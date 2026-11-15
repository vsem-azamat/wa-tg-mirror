package tg

import (
	"context"
	"errors"
	"strings"
	"time"

	ta "github.com/mymmrac/telego/telegoapi"

	"github.com/vsem-azamat/wa-tg-mirror/internal/bridge"
)

const (
	// maxRetries bounds how often one call is repeated after "too many requests".
	maxRetries = 5
	// maxRetryWait caps one wait demanded by Telegram.
	maxRetryWait = 2 * time.Minute
)

// retry runs call again while Telegram answers 429, waiting as long as it asks.
// Waiting keeps messages in order. Other errors are classified for the bridge.
func retry[T any](ctx context.Context, call func() (T, error)) (T, error) {
	for attempt := 0; ; attempt++ {
		v, err := call()
		var api *ta.Error
		if err == nil || !errors.As(err, &api) || api.ErrorCode != 429 || attempt == maxRetries {
			return v, classify(err)
		}
		wait := time.Second
		if api.Parameters != nil && api.Parameters.RetryAfter > 0 {
			wait = time.Duration(api.Parameters.RetryAfter) * time.Second
		}
		select {
		case <-ctx.Done():
			return v, ctx.Err()
		case <-time.After(min(wait, maxRetryWait)):
		}
	}
}

// classify wraps Telegram errors the bridge reacts to in its own sentinels.
func classify(err error) error {
	var api *ta.Error
	if !errors.As(err, &api) {
		return err
	}
	desc := strings.ToLower(api.Description)
	switch {
	case api.ErrorCode == 403, api.ErrorCode == 400 && strings.Contains(desc, "chat not found"):
		return errors.Join(bridge.ErrForbidden, err)
	case api.ErrorCode == 400 && strings.Contains(desc, "thread not found"):
		return errors.Join(bridge.ErrTopicGone, err)
	}
	return err
}
