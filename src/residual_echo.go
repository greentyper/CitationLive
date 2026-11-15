package main

func residualEchoDominates(nearRMS, cleanRMS float64) bool {
	return cleanRMS < 256 || cleanRMS < nearRMS*0.35
}

// In listening mode the music is ducked; distant speech can be below the old
// fixed floor of 256. Keep the relative echo test, with a lower noise floor.
// The stricter floor remains for assistant playback and its acoustic tail.
func suppressResidualEcho(nearRMS, cleanRMS float64, listening bool) bool {
	if !listening {
		return residualEchoDominates(nearRMS, cleanRMS)
	}
	return cleanRMS < 24 || cleanRMS < nearRMS*0.1
}
