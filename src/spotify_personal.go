package main

import (
	"strconv"
	"strings"
)

// These are feature names, not artist/title substrings. Catalogue URI discovery
// still comes from the authenticated account and requires Spotify ownership.
func spotifyPersonalPlaylist(name string) string {
	switch musicKey(strings.TrimSpace(name)) {
	case "daylist", "deilist", "deilista":
		return "daylist"
	case "dj", "didzhei", "didzheia", "didzheya":
		return "DJ"
	}
	return ""
}

func spotifyRecommendedPlaylist(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "daylist", "discover weekly", "release radar", "открытия недели", "радар новинок":
		return true
	}
	for _, prefix := range []string{"daily mix ", "микс дня "} {
		if suffix, ok := strings.CutPrefix(name, prefix); ok {
			n, err := strconv.Atoi(suffix)
			return err == nil && n >= 1 && n <= 6
		}
	}
	return false
}
