package main

import (
	"math"
	"strings"
	"unicode"
)

func spokenVolumeAdjustment(args M, text string) M {
	delta, ok := args["delta"].(float64)
	if ok && delta >= -6 && delta <= 6 && math.Trunc(delta) == delta {
		return args
	}
	direction := 0
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) }) {
		switch word {
		case "потише", "тише", "quieter", "tichšie":
			direction--
		case "погромче", "громче", "louder", "hlasnejšie":
			direction++
		}
	}
	switch {
	case direction < 0:
		return M{"delta": float64(-3)}
	case direction > 0:
		return M{"delta": float64(3)}
	default:
		return args
	}
}
