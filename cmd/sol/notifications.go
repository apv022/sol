package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/apv022/sol/internal/daemon"
)

// notificationsCmd is `sol notifications on|off|status`: desktop notifications of Inbox items,
// a per-device switch (default on). The Android shell posts its own and reports unsupported.
func notificationsCmd(ctx context.Context, c *daemon.Client, cmd string, asJSON bool) error {
	var st daemon.NotifyState
	switch cmd {
	case "notifications on", "notifications off":
		if err := c.Do(ctx, "POST", "/admin/v1/notifications", map[string]bool{"desktop": cmd == "notifications on"}, &st); err != nil {
			return err
		}
	default:
		if err := c.Do(ctx, "GET", "/admin/v1/notifications", nil, &st); err != nil {
			return err
		}
	}
	if asJSON {
		b, _ := json.MarshalIndent(st, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	state := map[bool]string{true: "on", false: "off"}
	fmt.Println("Desktop notifications:", state[st.Desktop])
	switch {
	case !st.Supported:
		fmt.Println("Not available here (no notifier, or Android, which posts its own).")
	case st.Override != "":
		fmt.Println("SOL_NOTIFY=" + st.Override + " overrides the setting.")
	}
	return nil
}
