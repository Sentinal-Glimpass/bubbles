# What's Coming

> Features that are fully designed and sitting on the shelf — waiting for the right moment to build.

---

## bubbles-net — federated cross-instance messaging

> **STATUS: PLANNED — ON HOLD. Not yet implemented. The design is decided; the code does not exist.**

Connect two separate `bubbles` instances — on different machines, different home networks, anywhere on the internet — so a bubble in your fleet can be `introduce`d to a bubble in a friend's fleet and they can `send` each other across the wire. Same inbox, same `send`, same address model. Just a bigger fleet.

---

### Concept

The fundamental insight: **bubbles-net is just another transport for `send`.**

Locally, `send` routes over an in-memory bus. Across the internet, it routes over an encrypted peer connection. The only new concept is a **namespace prefix on the address**: a bubble on another machine is `peer-id/0.1`. Your friend's `0.2` bubble is `friend-id/0.2` to you. Everything else — inbox, status, reply-grants, introduce — works identically because all of it is already keyed on address.

No new MCP tools. No new mental model. The one-verb philosophy holds.

---

### Why it fits

Bubbles bottoms out in one atom (a Bubble = address + session + `send`) and one verb (`send`). The address is just a dot-joined string; the kernel routes by it. Remote addresses are still dot-joined strings — they just have a prefix. The transport changes; the model does not.

This also means the entire consent and messaging machinery — `introduce`, reply grants, inbox, status — already handles the remote case without modification, because it was built around addresses, not machines.

---

### Identity & trust

Two identities, kept strictly separate:

**Routing identity — your ID.** A server-assigned, friendly, human-readable handle (think `tiger-violet-echo` — key-derived friendly words). It's purely a routing label: *here is how to reach you.* The server owns it for one purpose only.

**Trust identity — your keypair.** Generated on first run, stored at `~/.bubbles/identity` (Ed25519 for signing, X25519 for key exchange). Your public key is who you are. Trust never depends on the server — the server is explicitly untrusted. Recommended: make the ID self-certifying (derived from the public key), so the ID mathematically proves the key and impersonation is impossible.

No accounts. No passwords. Self-sovereign: your public key is your identity, forever.

---

### Pairing flow

Authenticator-style UX — the decided design:

1. **Join bubbles-net.** Press a key → keypair loads → outbound connection to the rendezvous server → you're online under your ID.
2. **Request an invite code.** Press a key → the server (which already verified your connection via your key) issues a single-use code that expires in ~10 minutes.
3. **Share it over a trusted channel.** Send the code to your friend over Signal, iMessage, whatever you already trust. The code travels out-of-band; the server never sees the relationship until both sides connect.
4. **Friend enters the code.** They press a key, type the code → connected. Both sides **pin each other's public key** (trust-on-first-use, same model as SSH). Pairing is done.

After pairing, your fleet knows a new peer exists. That's all it grants — see Introduce & consent below.

---

### Safety phrase (optional MITM defense)

After connecting, both screens show a **3-word phrase** derived from the two pinned public keys — for example, `amber-otter-canyon`.

You can ignore it entirely and get a fast, simple pairing flow. Or you can glance at it: if both screens show the same phrase, no man-in-the-middle is possible — not even a malicious rendezvous server — because the phrase is a deterministic function of the keys, and the keys never leave your machines.

This is Signal's "safety numbers," reduced to one optional glance. The only vulnerable window is the first handshake; the safety phrase seals it. After first contact, trust is pinned to keys forever.

---

### End-to-end encryption

All bubble-to-bubble messages are sealed with a session key derived from an X25519 handshake. The planned approach: **Noise protocol, IK handshake pattern** — the initiator already knows the responder's static public key from pairing, so authentication and key agreement happen in one round-trip. Fallback option: libsodium's `crypto_box` if Noise proves unwieldy in Go.

Forward secrecy via ephemeral keys per session. The rendezvous server — and any relay fallback — only ever sees ciphertext. There is no server-side key. There is no way to read the traffic at the infrastructure layer.

---

### Rendezvous server & NAT traversal

Each online instance holds one **outbound** connection to the rendezvous server. Because the connection is outbound, home NAT and firewalls just work — nothing to configure.

**v1 (planned, relay-first):** The server relays already-E2E-encrypted bytes between peers. It cannot read anything; it is a dumb pipe. Simpler to build and works on 100% of networks. The architecture is designed so the relay layer is a clean abstraction.

**v2 (later, P2P):** Exchange external IP:port candidates (STUN/ICE-style), attempt NAT hole-punching for a direct peer-to-peer connection, fall back to relay if hole-punching fails (~10% of networks, typically symmetric NAT). No UX change from v1 — the transport upgrade is transparent.

The server is always untrusted. Relay or not, it sees only ciphertext.

---

### Introduce & consent

**Pairing grants zero bubble access.** Knowing a peer exists does not mean any bubble can message any of their bubbles. Every bubble-to-bubble link requires a fresh `introduce`, and every introduce requires the receiving human to approve.

Flow: you introduce your `api` bubble (`0.1`) to your friend's `docs` bubble (`friend-id/0.2`). Their fleet view shows:

```
🤝 rishi wants to connect 0.1 (api) → your 0.2 (docs)   [a]llow / [d]eny
```

Only on approval can those two sessions send each other. A different pair of bubbles later is a fresh approval. Revoke anytime — the contact and its encrypted session drop immediately.

This is the same consent model as local `introduce`, extended across the internet. The least-authority principle holds: you get exactly the access you were explicitly granted, nothing more.

---

### v1 scope

The leaning for the first implementation: **relay-first, P2P-ready**. Ship the full UX — pairing, safety phrase, introduce, E2E-encrypted messaging — on a relay architecture that works on any network. Design the transport as a clean interface so direct P2P hole-punching drops in later as a v2 optimization with no user-visible change.

Note: this is the *leaning*, not a finalized decision. The feature is on hold; scope will be confirmed when implementation starts.

---

### Open questions

Things that are undecided and will need a decision before building:

- **ID format.** Key-derived friendly words (like WireGuard's mnemonic approach) vs. a longer opaque ID vs. something else. The self-certifying property (ID proves the key) is desirable but not confirmed.
- **Invite code format.** Length, character set, expiry time. Short enough to type; long enough to be unguessable in the 10-minute window.
- **Encryption library.** Noise protocol (e.g. `flynn/noise`) vs. libsodium bindings vs. raw `golang.org/x/crypto`. Noise IK is the cleanest fit but adds a dependency.
- **Hole-punching in v1 vs. v2.** Whether to attempt STUN/ICE in v1 (relay as fallback only) or defer entirely to v2.
- **Rendezvous server ops.** Who runs it, infrastructure, whether a self-hosted option ships alongside the OSS binary from day one.

---

### Business notes

This mirrors Tailscale's model. The server is untrusted and the traffic is E2E-encrypted, so a hosted rendezvous/relay service sees nothing about users' agent conversations. That makes it honest to run commercially.

Possible paths:

- **bubbles-net cloud** — hosted rendezvous + relay. Free personal tier (you and a few friends). Paid team tier: presence indicators, managed access control, SSO, many peers. Self-hostable for anyone who wants full control.
- **Open-core** — the protocol and client are MIT. Team and enterprise features (audit log, org-level access policies, SSO) are paid.
- **Hosted agent fleets** — managed VMs billed by compute, connecting into the same network via SSH-remote (a separate roadmap item). bubbles-net becomes the connective tissue for a hosted fleet offering. Here execution overhead is the cost of goods, so watch the lightweight-runtime space (e.g. isolate-based runtimes like agentOS that run agents in-process rather than VM-per-agent). The kernel's `Runner` seam means such a backend could slot in without touching the kernel or UX — relevant for the hosted/remote chapter, not the local IDE.

Short version: ship the OSS protocol, run the relay as a service, charge for the team layer. Keep the self-hosted path honest.

---

*bubbles-net was designed in June 2026. Implementation has not started.*
