//go:build !youtube_dial

package main

import "net/http"

func registerYouTubeDIAL(_ *http.ServeMux) func() {
	return func() {}
}
