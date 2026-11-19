// Package e2e holds end-to-end tests of the daemon's networking: several
// in-process devices ("daemons-lite": a keyring, a session manager and a
// clearnet transport each) that join, pair, hold sessions and heal over real
// loopback TCP. It has no non-test code; the tests prove the product works.
package e2e
