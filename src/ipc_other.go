//go:build !linux

package main

func cleanDsnoopIPC() error {
	return nil
}
