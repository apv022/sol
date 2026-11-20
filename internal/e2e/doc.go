// Package e2e holds end-to-end tests of the daemon's networking: several
// in-process devices ("daemons-lite": a keyring, a feed store with its
// contacts, sharing, pins and blob-reference views, a blob store, a
// replicator, a blob service, a session manager and a
// clearnet transport each) that join, pair, hold sessions, replicate feeds
// and blobs, and heal over real loopback TCP. It has no non-test code; the tests prove
// the product works.
package e2e
