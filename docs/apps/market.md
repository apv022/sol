# Market: stores on your own node

> **App design, not part of Sol.** A separate program built on [../APPS.md](../APPS.md). It is
> written down here to check that the API is enough. Pattern: **transact**, plus **subscribe**
> for catalogues.

A seller runs their store on their own node. Listings are public and signed, buyers find them
through Nostr and directories, orders are signed messages between buyer and seller, and payment
is Monero, straight to the seller's wallet. There is no central server, no fee taker, and no
company in the middle.

## Lessons from earlier peer-to-peer markets

OpenBazaar (2016 to 2021) tried this and hit three problems. The design answers each one:

| Problem | Answer here |
|---|---|
| A seller's store vanished whenever their computer was off | Listings are signed public content, so bigger nodes and Nostr relays can serve the catalogue while the seller is off. Orders wait in the buyer's outbox, or in a mailbox node, until the seller is back |
| Running a node was hard | Sol handles it: one binary, outbound only, works behind CGNAT |
| Finding anything was hard | Listings are also Nostr marketplace events (NIP-15 stalls and products), so existing Nostr market clients and their search can show them, plus Sol directories |

## Pieces

### 1. Store and listings (subscribe)

- The store runs on the seller's node and is published on the app's **public onion**
  (`wants.public`, which the seller approves). Buyers need no Sol: Tor Browser opens the store.
- Each listing is signed with the store's key: Sol's per-app key (primitive 4), or the seller's
  Nostr key when listings also go to Nostr. Both keep the store unlinkable to the seller's
  private node.
- The catalogue is pull: anyone can fetch it, mirror it, and check the signatures.

### 2. Orders (transact)

- An order is a signed message: items, quantity, shipping details **encrypted to the seller's
  key**, and a fresh public key for the buyer for this order alone. That per-order key lets the
  buyer prove later that they placed it, without an account and without linking orders.
- Delivery: the buyer's app posts the order to the store's public onion. If the store is offline,
  the order waits in the buyer's outbox and is retried, or goes to a mailbox node the seller
  named (it stays encrypted, so the mailbox can't read it).
- Sol's role is only reach. Buyers are strangers, so the **app** authenticates them, by those
  signatures. Sol's peer identity (primitive 1) only matters for sellers who also sell to paired
  peers.

### 3. Payment (Monero)

- **First version, direct payment.** The seller's wallet makes a new subaddress per order. The
  store watches for payment with a **view-only** wallet: it can see money arrive but cannot spend
  it. The spend key never touches the store, the app or Sol.
- **Later, escrow.** 2-of-3 Monero multisig between buyer, seller and a moderator both chose. It
  protects buyers, but setting it up is complex and easy to get wrong, so it should not block the
  first version.
- Sol never holds a wallet, a key or a balance. Payments are entirely the app's.

### 4. Reputation

- Reviews are signed with the buyer's per-order key, so only someone who placed (and paid for) an
  order can review it. That limits fake reviews without accounts.
- Reviews are public signed content, mirrored like listings.

## What it needs from Sol

| Need | Primitive |
|---|---|
| A public store that can't be linked to the seller's node | 3. Manifest with `wants.public`; a public onion per app, as public services have today |
| Signed listings | 4. App key, or the seller's own Nostr key |
| Optional: sell to paired peers, with their identity | 1. Who is calling |
| Optional: a seller's own mailbox node | 2. Dial a peer's app (the seller's two machines are peers) |

## Risks

- **Illegal listings.** A Tor and Monero market will attract them. Stores are run by sellers, not
  by the app's authors, and there is no central index, which helps. Directories should apply a
  content policy (spec.md 9.5), and the app's authors should take legal advice before release.
  This is a bigger exposure than any other app here.
- **Scams.** Without escrow, buyers trust sellers. Reputation and starting with small orders
  are the first-version answer, escrow the later one.
- **Wallet safety.** The view-only design keeps a compromised store from stealing funds. Never
  relax it.

## Open questions

1. Listing format: NIP-15 as is, or NIP-15 plus fields it lacks (digital goods, shipping zones)?
2. Mailbox nodes: one per seller, or shared community mailboxes?
3. Moderators for escrow: how buyers and sellers find and choose them.
4. Dispute flow for the escrow version.
