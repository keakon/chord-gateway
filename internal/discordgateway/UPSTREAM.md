# Upstream and local lifecycle maintenance

Source: [`github.com/disgoorg/disgo/gateway`, tag `v0.19.6`](https://github.com/disgoorg/disgo/tree/v0.19.6/gateway) (pinned in `go.mod` as `github.com/disgoorg/disgo v0.19.6`; wire types in `protocol.go` are aliases to that pinned module).
The local fork retains the connection supervisor and command limiter. Wire types
and constants are aliases to the same pinned upstream module in `protocol.go`.
The Apache-2.0 license is retained in `LICENSE`; that upstream release has no NOTICE.
Discord REST and public discord types remain dependencies on the original pinned module.

Local changes:

- `New` requires an explicit lifecycle context. One supervisor owns dialing and
  generation changes; listener and heartbeat workers never reconnect themselves.
- Open submits an opening request and waits for authenticated READY/RESUMED.
  Its argument cancels only that wait; cancel the constructor context or call Close
  to end the connection. Close is permanent and idempotent and joins all workers.
- Every retry, Identify quota wait, dial, HELLO/READY wait, and heartbeat belongs to
  the lifecycle. Each connection reports termination at most once through a
  capacity-one result channel. Worker WaitGroup additions precede their only Wait.
- State and event callbacks include the producing generation; callbacks execute
  outside the state mutex and must return promptly. Do not synchronously call Close
  from a callback, as Close joins the worker delivering it. READY validation runs
  before Ready is published; a rejected identity is fatal.
- Session, sequence, Resume URL and heartbeat observations share one mutex.
  Session getters return copies. Transient loss preserves resumable state;
  InvalidSession applies its resume flag and a random 1-5 second delay. Normal
  permanent shutdown clears session state.
- Only uncompressed JSON is supported; unused zlib/zstd transports and Identify
  limiter implementations are removed. Incoming messages have a 16 MiB read limit
  (a client resource policy). Decoder errors omit upstream payload-bearing text.
  Data writes have five-second deadlines. Shutdown closes the socket directly to
  interrupt I/O and joins the reader.
- Single-shard Identify uses a five-second minimum spacing and the initial
  `/gateway/bot` remaining/reset budget. Resume never spends Identify quota.
  HTTP 401/403 and Gateway 4004/4010-4014 are fatal.
- WithIntents sets precisely the requested intents. Runtime OS/browser/device
  defaults are explicit. Protocol flags come from the upstream type aliases.
- Gateway frame and session debug logging is removed; raw messages/tokens are not
  forwarded to logging. The root adapter also filters SDK log attributes.
- The gateway command limiter retains its upstream interface and limit semantics,
  with synchronized Reset and context-cancellable serialization to avoid races
  between sends and reconnects.

No voice, bot cache, or agent runtime packages are copied. `CloseWithCode` is
removed because its parameters had no effect. `Close(ctx)` permanently stops and
joins workers; the argument does not bound that join. Keep callbacks prompt.

When updating disgo, compare supervisor, heartbeat, resume and limiter semantics
against this fork, and rerun its lifecycle, privacy and message-limit tests.
Type aliases follow the pinned dependency; they do not remove fork maintenance.

## REST limiter compatibility hook

The upstream REST Client is used without copying or modifying its implementation.
Its default v0.19.6 limiter starts a cleanup ticker which `Close` never stops,
retains a mutex during Close, and accesses global/bucket observations without
consistent synchronization. The root adapter injects `discord_ratelimiter.go`
through the public `rest.WithRateLimiter` hook. This account-scoped limiter keeps
route and global 429 scheduling, uses cancellable waits, synchronizes all mutable
state, and needs no cleanup goroutine. Adapter test clients use the same hook.

`discord_http.go` only normalizes explicit 429 retry delays and global scope from
headers or the bounded JSON response body, retaining fractional seconds and the
response body for the original REST Client. It leaves successful responses and
their reset headers unchanged; the owned limiter parses those reset durations
without truncating fractions. It does not replay requests or retry network/5xx
outcomes. Only explicit 429 rejection is retryable.

## Resume replay ordering

Discord sends missed dispatches **before** the final RESUMED event. During a
verified session Resume, `WithEventCaptureHandler` runs on transport arrival and
returns a delivery closure. The adapter freezes the common ingress operation
and control target at this point. Up to 1024 useful message closures are held by
the existing connection listener, without occupying the common normalization
workers. Metadata can be ignored and fetched from REST after reconnect.

RESUMED publishes Ready and then delivers captured messages in receive order to
the existing bounded normalization queue. A replay interrupted before delivery
restores the previous Resume checkpoint. Buffer overflow (beyond 1024 useful
message closures) returns the explicit
fatal `ErrReplayCapacityExceeded`, with that checkpoint restored before the close
handler observes it; permanent shutdown then clears session data. It never
silently skips buffered events or enters an unbounded replay retry loop.

Captured messages already submitted to normalization belong to the adapter
lifecycle, so a later transient generation change alone does not discard them.
This is still a volatile admission boundary: process exit, full queues, or a
platform-unavailable admission result before process-local intake accepts the event
cannot be repaired solely by Gateway Resume. Malformed-frame decode errors also
omit the original payload, keeping SDK error attributes free of message bodies.

## Identify quota window refresh

`reset_after` is a countdown to the current window's reset, not a period that
can be repeated. The adapter provides `WithSessionStartLimitRefresh` using the
original authenticated REST `GET /gateway/bot` with `rest.WithCtx`. Once quota is
exhausted the supervisor waits to the known reset, reads the current budget, and
if necessary waits to the newly returned reset. Resume neither refreshes nor
spends this budget. HTTP 401/403, unsupported sharding, and malformed quota data
terminate; temporary provider errors follow the cancellable supervisor backoff.

Zero reset duration is allowed, but a refreshed zero-remaining/zero-reset pair is
fatal to prevent a busy loop. Standalone users without a refresh provider may
refill once at the initial reset, then must provide fresh quota rather than
silently repeating the initial countdown. All Identify attempts remain at least
five seconds apart.
