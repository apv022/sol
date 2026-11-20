// Package e2e holds end-to-end tests of Sol, all in one process over real loopback TCP.
//
// The older tests build "daemons-lite" by hand (a keyring, a feed store with its contacts,
// sharing, pins and blob-reference views, a blob store, a replicator, a blob service, a session
// manager and a clearnet transport each) to prove the networking: join, pair, sessions,
// replication of feeds and blobs, healing. The newer ones start real daemons (internal/daemon)
// and prove the product through the app API: the soltest conformance suite (the scenarios
// memsol passes) run through the real client, and the builtin Files app syncing between two
// people's devices through the real API and UI router. It has no non-test code.
package e2e
