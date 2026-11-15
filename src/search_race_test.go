package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSearchRaceReturnsSuccessAndCancelsSlowProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stopped := make(chan struct{})
	slow := func(ctx context.Context, query string) (M, error) {
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}
	fast := func(ctx context.Context, query string) (M, error) {
		return M{"query": query, "ok": true}, nil
	}
	query := "все слова запроса без изменений"
	result, err := firstSearchSuccess(ctx, query, slow, fast)
	if err != nil || result["query"] != query || result["ok"] != true {
		t.Fatalf("result=%v error=%v", result, err)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("losing provider was not canceled")
	}
}

func TestSearchRaceFailureDoesNotHideSuccess(t *testing.T) {
	failed := make(chan struct{})
	first := func(context.Context, string) (M, error) {
		close(failed)
		return nil, errors.New("no organic results")
	}
	second := func(ctx context.Context, query string) (M, error) {
		<-failed
		return M{"ok": true}, nil
	}
	result, err := firstSearchSuccess(context.Background(), "query", first, second)
	if err != nil || result["ok"] != true {
		t.Fatalf("result=%v error=%v", result, err)
	}
}

func TestSearchRaceHonorsCancellationAndAllFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wait := func(ctx context.Context, query string) (M, error) { <-ctx.Done(); return nil, ctx.Err() }
	if _, err := firstSearchSuccess(ctx, "query", wait, wait); err == nil {
		t.Fatal("cancellation lost")
	}
	fail := func(context.Context, string) (M, error) { return nil, errors.New("failed") }
	if _, err := firstSearchSuccess(context.Background(), "query", fail, fail); err == nil {
		t.Fatal("failures lost")
	}
}
