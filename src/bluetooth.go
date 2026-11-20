package main

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

var bluetoothMu sync.Mutex
var bluetoothANSI = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func runBluetoothCommand(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bluetooth-cli", "--restricted")
	cmd.Stdin = strings.NewReader(command + "\n")
	out, err := cmd.Output()
	return string(out), err
}

func bluetoothEnabled(ctx context.Context) (bool, error) {
	out, err := runBluetoothCommand(ctx, "is-enabled")
	if err != nil {
		return false, errors.New("Не удалось прочитать состояние Bluetooth на колонке.")
	}
	for _, line := range strings.Split(bluetoothANSI.ReplaceAllString(out, ""), "\n") {
		_, state, ok := strings.Cut(line, "Adapter enabled:")
		if !ok {
			continue
		}
		switch strings.TrimSpace(state) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, errors.New("Колонка вернула неизвестное состояние Bluetooth.")
}

func controlBluetooth(ctx context.Context, action string) M {
	if action != "on" && action != "off" && action != "status" {
		return M{"ok": false, "error": "Укажите действие Bluetooth: on, off или status."}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	bluetoothMu.Lock()
	defer bluetoothMu.Unlock()

	enabled, err := bluetoothEnabled(ctx)
	if err != nil {
		return M{"ok": false, "error": err.Error()}
	}
	wanted := action == "on"
	if action == "status" || enabled == wanted {
		return M{"ok": true, "enabled": enabled}
	}
	command := "disable"
	if wanted {
		command = "enable"
	}
	for attempt := 0; attempt < 6; attempt++ {
		// The controller can reject requests while an earlier transition settles.
		// Read its actual state after each attempt before reporting success.
		_, _ = runBluetoothCommand(ctx, command)
		timer := time.NewTimer(650 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return M{"ok": false, "error": "Колонка не подтвердила смену состояния Bluetooth."}
		case <-timer.C:
		}
		if enabled, err = bluetoothEnabled(ctx); err == nil && enabled == wanted {
			return M{"ok": true, "enabled": enabled}
		}
	}
	return M{"ok": false, "error": "Колонка не подтвердила смену состояния Bluetooth."}
}
