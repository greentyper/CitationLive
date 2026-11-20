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
	if action != "on" && action != "off" && action != "status" && action != "pair" {
		return M{"ok": false, "error": "Укажите действие Bluetooth: on, off, status или pair."}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	bluetoothMu.Lock()
	defer bluetoothMu.Unlock()

	if action == "pair" {
		if result := changeBluetoothPower(ctx, "on"); result["ok"] != true {
			return result
		}
		if _, err := wampCallTimeout(3*time.Second, "com.harman.bluetooth.pairing", M{}, true); err != nil {
			return M{"ok": false, "error": "Не удалось запустить сопряжение Bluetooth на колонке."}
		}
		state, err := wampCallTimeout(2*time.Second, "com.harman.stateGet", M{})
		if err == nil && len(state) > 4 {
			fields, _ := state[4].(map[string]any)
			bluetooth, _ := fields["bluetooth"].(map[string]any)
			if bluetooth["state"] == "pairing" {
				return M{"ok": true, "enabled": true, "pairing_requested": true}
			}
		}
		return M{"ok": false, "error": "Колонка не подтвердила режим сопряжения Bluetooth."}
	}
	return changeBluetoothPower(ctx, action)
}

func changeBluetoothPower(ctx context.Context, action string) M {
	enabled, err := bluetoothEnabled(ctx)
	if err != nil {
		return M{"ok": false, "error": err.Error()}
	}
	wanted := action == "on"
	if action == "status" {
		return M{"ok": true, "enabled": enabled}
	}
	if enabled == wanted {
		return M{"ok": true, "enabled": enabled, "changed": false}
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
			return M{"ok": true, "enabled": enabled, "changed": true}
		}
	}
	return M{"ok": false, "error": "Колонка не подтвердила смену состояния Bluetooth."}
}
