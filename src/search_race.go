package main

import (
	"context"
	"fmt"
)

type searchLookup func(context.Context, string) (M, error)

// Race equivalent read-only lookups. Losing requests are canceled; a failure
// cannot beat another provider's success. The caller chooses provider tiers.
func firstSearchSuccess(ctx context.Context, query string, lookups ...searchLookup) (M, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		result M
		err    error
	}
	results := make(chan outcome, len(lookups))
	for _, lookup := range lookups {
		go func(lookup searchLookup) {
			result, err := lookup(ctx, query)
			results <- outcome{result, err}
		}(lookup)
	}
	errors := make([]error, 0, len(lookups))
	for range lookups {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-results:
			if result.err == nil {
				return result.result, nil
			}
			errors = append(errors, result.err)
		}
	}
	return nil, fmt.Errorf("search providers failed: %v", errors)
}
