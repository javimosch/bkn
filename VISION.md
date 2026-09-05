# bkn — Vision

## North star

**A backend you own, small enough to read, that outlives the tool that wrote it.**

One static binary gives a project the things every backend re-invents — documents,
settings, identity, files, an event log, a scheduler, signed webhooks — over an
embedded SQLite file. No database server, no runtime to install, no dashboard, no
account. When it stops being useful, what remains is a `.db` file of plain JSON
documents and a directory of ordinary JavaScript. Nothing has to be migrated off,
because nothing was ever locked in.

## The one belief everything hangs on

**Most of what a backend does is not core — it is a script.** Stripe webhooks,
form handling, CSV exports, i18n, redirects, feature flags, a CMS: each is a
handler over documents plus a public URL, not a feature the core should grow.

The trade against PocketBase is deliberate and opposite: **no admin UI**, and a
sandboxed JS runtime as the extension point instead of compiled hooks. That keeps
the core small enough that one person can read it, and puts the parts that change
often in a file you can edit without a compiler.

The evidence, not the hope: nine domains of an 85k-line Node/Express/Mongo
backend moved onto bkn as roughly 1,700 lines of scripts, with almost nothing
leaking back into Go.

## What gets in

The north-star metric below implies an ambition — a complex system should get
**smaller** on bkn, not merely become possible — and that ambition has an
obvious failure mode: it is this project's own origin. superbackend reached 85k
lines because the core had no escape hatch and every need became a feature.
Absorbing requirements one SQL construct at a time makes bkn SQLite with extra
steps.

So the core moves under one rule:

> **Admit a primitive that removes a class of application code from every
> embedder. Refuse a query feature that only moves application code into bkn.**

Atomic field updates remove read-modify-write races from everyone who writes
concurrently — admit. Joins move query composition into bkn and remove nothing
from the caller — refuse, denormalize, and pay the write amplification
knowingly.

This is the only way the six-verb line in `AGENTS.md` moves. It is where the
surface sits today, not a promise that it never moves — but what it must never
become is a query language.

## Why this shape

| value | what it decided here |
|---|---|
| Own your execution | one static binary on your box; no vendor runtime, no control plane |
| Avoid irreversible lock-in | a SQLite file of JSON documents and plain JS — leaving costs nothing |
| Durability over novelty | the **contract** is the product; the implementation is replaceable |
| Diversity over uniformity | two independent implementations, one suite (see below) |
| Margins over optimization | prefer a flat, boring latency curve to a fast average with a bad tail |
| Territory over cloud | runs on a small VM you control; cold start is milliseconds, idle RSS single-digit MB |
| Commons over platforms | open source and self-hosted; a hosted version must never be the only way to run it |

"Margins over optimization" is not decoration here. Benchmarked against the MFL
port, bkn wins p50 on writes by roughly 8x and loses p99 by roughly 8-11x: it
absorbs a write fast and pays in a multi-second tail. That is a real trade to
make deliberately, not a number to maximize.

## What we know, and what we don't

Known, because it was measured:

- A clean-room reimplementation in another language (`machin-bkn`) passes the
  same **113-assertion live suite**, unmodified. The contract is portable, not a
  description of one codebase.
- Read paths are a wash between the two; the write path trades p50 for p99.
- 20.1 MB stripped / 17 MB idle RSS for the Go build; 7.8 MB static / 3.3 MB for
  the MFL one.
- Two *other people's* backends were fit-checked against `store`, counting
  distinct SQL statements and how many exceed what it offers. superlandings-go
  (8k LOC, 8 tables): 48 statements, 3 beyond, 1 relational. automaintainer-saas-panel
  (131k LOC, 20 tables): 202 statements, 33 beyond (16%), only ~10 relational (5%).
  Of the large one's 33 outliers, 8 are atomic updates (`log || ?`, `n + 1`), 4 a
  sort tiebreak, 2 retention trims, 2 compare-and-set guards, 1 a status rollup,
  and 9 plain `COUNT(*)` that `list` already answers with `total`. **The distance
  between bkn and a complex system is mostly atomicity and summarisation, not a
  query language** — a gap that closes with primitives, which is what the
  admission rule allows.
- That prediction was then tested by closing it. Atomic update operators with
  compare-and-set, retention declared on the collection, and grouped counts took
  the same codebase from 33 statements beyond the surface to **11**, or 5% of
  all 202. Every one of the 11 is relational or presentational — 8 joins, 2
  `LIKE`, and one two-key display sort — and every one was refused rather than
  absorbed. The core grew by three primitives and no query language.

Not known, and worth saying plainly:

- **Nobody is running this yet.** Zero stars, zero external users at the time of
  writing. Every claim above is about the artifact, none of it about demand.
- Whether "verified handlers you can install and re-verify" is worth paying for
  is untested — `bkn-recipes` exists to find out, and silence there proves
  nothing while bkn has no users.
- A 113-assertion suite of sequential requests certified a build that died at
  concurrency 2. Coverage is not correctness; `test/concurrency.sh` exists
  because of that.

## Non-goals

- **No admin UI.** The CLI is the interface; agents drive it, browsers don't.
- **No hosted BaaS.** Running other people's data is the opposite of the point,
  and a fat server is the least defensible thing here.
- **No feature race** with Supabase/Firebase/PocketBase. Small and readable is
  the feature.
- **No LLM inside the tool.** Scripts are deterministic; intelligence stays with
  the operator.

## North-star metric

**Application code that left the core.** Not stars, not requests per second: how
much of a real backend can move out of your language and into scripts without
leaking back. The one datapoint so far is 85k lines of Node reduced to ~1,700
lines of handlers across nine domains. If that ratio holds for someone else's
backend, bkn is the right size. If it doesn't, the core is wrong — not their app.

## This repo

`bkn` is the reference implementation: Go, embedded SQLite, goja for scripts. It
defines the contract that `test/` asserts. See
[machin-bkn](https://github.com/javimosch/machin-bkn) for the independent
implementation that keeps this honest.

> If you don't own the stack, you are part of someone else's system.
> If it can't last, it won't liberate anyone.
