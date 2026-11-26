package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"strings"

	"github.com/apv022/sol/internal/daemon"
)

// nearbyCmd is `sol nearby ad|match|interfaces|report|ble-ad|ble-match|ble-report`: the Android
// shell's Wi-Fi Direct and Bluetooth LE plumbing (design/PROTOCOL.md §Nearby "Wi-Fi Direct",
// "Bluetooth LE"), not a person's operation (that is `sol do sol.nearby --wifi_direct|--ble
// on|off`). Requests that carry a group passphrase
// come on stdin ("-"), never in argv; replies are JSON.
func nearbyCmd(ctx context.Context, c *daemon.Client, cmd string, args []string, stdin io.Reader, out io.Writer) error {
	readIn := func(v any) error {
		if len(args) != 1 || args[0] != "-" {
			return errors.New("usage: sol " + cmd + " - < request.json")
		}
		if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
			return errors.New(cmd + ": bad JSON on stdin")
		}
		return nil
	}
	var res any
	switch cmd {
	case "nearby ad":
		var in json.RawMessage
		if len(args) > 0 {
			if err := readIn(&in); err != nil {
				return err
			}
		}
		var ad daemon.WifiDirectAd
		if err := c.Do(ctx, "POST", "/admin/v1/nearby/wifi-direct/ad", in, &ad); err != nil {
			return err
		}
		res = ad
	case "nearby match":
		var in json.RawMessage
		if err := readIn(&in); err != nil {
			return err
		}
		var plan json.RawMessage
		if err := c.Do(ctx, "POST", "/admin/v1/nearby/wifi-direct/match", in, &plan); err != nil {
			return err
		}
		res = plan
	case "nearby ble-ad", "nearby ble-match":
		var in json.RawMessage
		if err := readIn(&in); err != nil {
			return err
		}
		var reply json.RawMessage
		if err := c.Do(ctx, "POST", "/admin/v1/nearby/ble/"+strings.TrimPrefix(cmd, "nearby ble-"), in, &reply); err != nil {
			return err
		}
		res = reply
	case "nearby ble-report":
		if len(args) == 0 {
			return errors.New("usage: sol nearby ble-report <state> [detail]")
		}
		var st json.RawMessage
		if err := c.Do(ctx, "POST", "/admin/v1/nearby/ble/report",
			map[string]string{"state": args[0], "detail": strings.Join(args[1:], " ")}, &st); err != nil {
			return err
		}
		res = st
	case "nearby interfaces":
		addrs := []netip.Addr{}
		for _, a := range args {
			ip, err := netip.ParseAddr(a)
			if err != nil {
				return errors.New("usage: sol nearby interfaces [IPv4 address...] (none clears them)")
			}
			addrs = append(addrs, ip)
		}
		var st json.RawMessage
		if err := c.Do(ctx, "POST", "/admin/v1/nearby/interfaces", map[string]any{"addrs": addrs}, &st); err != nil {
			return err
		}
		res = st
	case "nearby report":
		if len(args) == 0 {
			return errors.New("usage: sol nearby report <state> [detail]")
		}
		var st json.RawMessage
		if err := c.Do(ctx, "POST", "/admin/v1/nearby/wifi-direct/report",
			map[string]string{"state": args[0], "detail": strings.Join(args[1:], " ")}, &st); err != nil {
			return err
		}
		res = st
	default:
		return errors.New("usage: sol nearby ad [-] | match - | interfaces [IPv4...] | report <state> [detail] | ble-ad - | ble-match - | ble-report <state> [detail]")
	}
	enc := json.NewEncoder(out)
	return enc.Encode(res)
}
