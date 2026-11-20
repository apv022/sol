package daemon

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/apv022/sol/internal/apphost"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/session"
	"github.com/apv022/sol/sdk/go/sol"
)

// BackupWords returns the recovery words while the paper backup is unconfirmed (only on the
// admin device that created the identity). The caller shows them and never logs them.
func (d *Daemon) BackupWords() ([]string, error) {
	w, ok := d.Keyring.PendingWords()
	if !ok {
		return nil, sol.Errorf("not_found", "no unconfirmed words here: the backup was confirmed, or this device did not create the identity")
	}
	return w, nil
}

// ConfirmBackup records that the person wrote the words down; they are deleted from disk.
func (d *Daemon) ConfirmBackup() error {
	if err := d.Keyring.ConfirmBackup(); err != nil {
		if errors.Is(err, identity.ErrNoBackup) {
			return sol.Errorf("not_found", "no unconfirmed words here")
		}
		return err
	}
	d.log.Info("recovery words confirmed and removed from this device")
	return nil
}

// RestoreOptions are the choices of Restore.
type RestoreOptions struct {
	// Words are the 24 recovery words, separated by spaces.
	Words string
	// Name is the person's name; empty means the name in the recovered person certificate
	// (required without From).
	Name string
	// Device names this device.
	Device string
	// From is a pair or join link of a device that may still know this person: a surviving
	// device of mine, or a contact's. Restore asks it for the newest identity state it holds
	// (the recover handshake) and starts above it, so contacts accept the restored identity
	// even if this clock is behind. Empty restores from the words alone.
	From   string
	Config Config
	// Builtins are installed as apps.
	Builtins map[string]apphost.Builtin
	Log      *slog.Logger
}

// Restore sets up a fresh data directory from the recovery words after every device was lost:
// a new device holding a new person key, under the same person ID. It runs without a daemon.
func Restore(ctx context.Context, dir string, o RestoreOptions) error {
	if _, err := identity.Open(keysDir(dir)); err == nil {
		return ErrExists
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	words := strings.Fields(o.Words)
	entropy, err := identity.WordsToEntropy(words)
	if err != nil {
		return err
	}
	r, err := identity.RecoveryKey(entropy)
	clear(entropy)
	if err != nil {
		return err
	}
	if err := prepareDir(dir, &o.Config, o.Builtins); err != nil {
		return err
	}
	var pcSeq, dlSeq uint64
	name := o.Name
	if o.From != "" {
		pc, dl, err := recoverFrom(ctx, dir, o, r)
		if err != nil {
			return err
		}
		pcSeq, dlSeq = pc.Seq, dl.Seq
		if name == "" {
			name = pc.Name
		}
	}
	if name == "" {
		return errors.New("daemon: a name is required when restoring without --from")
	}
	_, err = identity.RestoreAbove(keysDir(dir), words, name, o.Device, pcSeq, dlSeq, time.Now())
	return err
}

// recoverFrom asks the device of o.From for the newest identity state of r's person, over the
// configured transports (listening only on loopback while it runs).
func recoverFrom(ctx context.Context, dir string, o RestoreOptions, r ed25519.PrivateKey) (identity.PersonCert, identity.DeviceList, error) {
	l, err := pairing.ParseLink(o.From)
	if err != nil {
		return identity.PersonCert{}, identity.DeviceList{}, err
	}
	_, dk, err := ed25519.GenerateKey(nil)
	if err != nil {
		return identity.PersonCert{}, identity.DeviceList{}, err
	}
	tcs := make([]TransportConfig, len(o.Config.Transports))
	for i, tc := range o.Config.Transports {
		tc.Listen, tc.Advertise = "127.0.0.1:0", nil
		tcs[i] = tc
	}
	ts := newTransports(dir, tcs, dk, o.Log)
	for _, t := range ts {
		if err := t.Start(ctx); err != nil {
			o.Log.Warn("transport did not start", "transport", t.Name(), "err", err)
		}
		defer t.Stop()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	pc, dl, err := session.Recover(ctx, ts, l.Endpoints, r)
	if errors.Is(err, session.ErrNothingKnown) {
		return pc, dl, errors.New("daemon: that device knows nothing of this person (is it yours, or a contact's?)")
	}
	return pc, dl, err
}
