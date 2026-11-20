// Package feeds is the daemon's feed store (design/API.md §3,
// design/PROTOCOL.md §Feeds on the wire) and the small Sol-internal views
// built on feeds: the contacts list and the sharing records.
//
// A feed <person>/<app>/<name> is, under the hood, one signed chain per
// device of its owner. Each entry is signed by the device key that wrote it
// (E = lp("sol entry v1") | lp(addr) | u64(seq) | lp(prev) | u64(unix_ms) |
// lp(D) | lp(app) | lp(body); hash = sha256(E | sig)), so two devices
// appending offline never conflict. The store also keeps, per feed, a local
// cursor pos: 1, 2, 3... in arrival order across chains.
//
// On disk, under the store root:
//
//	<person>/<app>/<name>/<device ID>.log   one chain, append-only records
//	<person>/<app>/<name>/pos.idx           arrival order: 40-byte records
//	                                        (32-byte device key | u64 seq)
//	incoming.json                           what contacts share with me
//
// A chain record is u32 len | JSON Wire | u32 CRC-32C. Directories are 0700,
// files 0600.
//
// Invariants:
//
//   - Every stored chain is linear: seq 1, 2, 3... each prev the hash of
//     the one before. Accept refuses anything else (gaps are reported with
//     the missing range, forks and bad prevs are refused).
//   - Only entries from a device in the owner's newest device list are
//     accepted. Entries stored before a device was removed stay; nothing
//     new from it is accepted (the removal point is what this store held
//     when it learned of the removal).
//   - Crash safety: a record is written chain first, index second, each
//     at the known end of its file. On open, each chain keeps its longest
//     valid prefix (a torn or corrupt tail is truncated) and the index
//     keeps its longest prefix consistent with the chains; entries missing
//     from it are appended (so the pos of entries lost in a crash may be
//     reassigned, never duplicated).
//   - fsync policy: Append (a local write, irreplaceable) fsyncs the chain
//     and the index before returning. Accept does not; replication calls
//     Flush after each batch, and anything a crash loses is fetched again,
//     because the haves sent on reconnect come from the store itself.
//   - The store never interprets an entry body beyond checking it is
//     canonical JSON of at most MaxBodySize bytes. The internal views
//     interpret only feeds of InternalApp.
//
// The store publishes EventEntry for every stored entry on Bus; the views
// publish EventFeed when a feed becomes readable or unreadable to me.
package feeds
