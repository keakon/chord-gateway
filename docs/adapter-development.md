# Adding an IM adapter

Gateway owns transport integration, authorization and forwarding. Chord owns sessions, execution and saved results. Intake is bounded and process-local; the [delivery contract](./operations.md#message-delivery-and-restarts) explicitly allows loss or duplication across Gateway restarts.

## Keep a platform integration small

Implement identity verification, connection lifecycle, event normalization and sending. Reuse `IngressNormalizer` for bounded asynchronous work, common admission/dispatch for authorization and receipts, and `IMTextSender` for segmentation, deadlines and send diagnostics. A new adapter should not add another inbox, deduplication database, migration, command router or recovery queue.

`im_adapter_host.go` defines account, polling and socket host interfaces; Telegram and Slack use these boundaries. Prefer the smallest required interface to direct access to `NotificationRouter` internals. Older adapters still have concrete-router dependencies; remove these when changing that path instead of copying them into another integration. Platform event IDs and verified account/chat identity enter a common `IngressEnvelope`. Platform ACK timing remains in the adapter: enqueueing normalization is not acceptance, and acceptance is not Chord execution.

Use an optional readiness provider only for an additional local prerequisite, such as WeChat reply context. Inspect local state without I/O and notify outside locks. Do not add a WebSocket readiness gate to independently usable HTTP sends merely because another platform has one. `IMTextMetricsProvider` implementations should have compile-time assertions.

Share small policy-free functions such as UTF-16 measurement and backoff jitter. Keep protocol-specific reconnect, authentication, heartbeat and rate-limit rules separate. Start with the existing SDK when its lifecycle meets the requirements; a fork needs a concrete defect, pinned origin, retained license, a small change surface and regression tests. The Discord fork reuses upstream wire types and keeps only the needed lifecycle and limiter implementation; it still needs review against upstream when the dependency changes.

## Test at the relevant boundary

Use table tests for parsing, identities and text conversion; these are candidates for `t.Parallel()`. Test shared admission, ordering, capacity and receipts once in the common layer, with a small integration test per adapter proving that it uses that layer. Fake HTTP/WebSocket peers test protocol behavior and cancellation without real credentials.

Use channels and `testing/synctest` for internal timing. Real network/process tests still need deadlines, but synchronize on actual events before asserting. Join workers before fixtures are removed. The root `TestMain` isolates machine-wide locks per process; tests running in parallel still need distinct accounts unless testing contention. Keep tests mutating environment or package globals serial.

Run these gates separately:

```sh
./scripts/check-go-quality.sh
# Includes test coverage, formatting, vet and staticcheck.
go test -race -short -timeout=5m ./...
CHORD_GATEWAY_CONTRACT_CHORD_REPO=/path/to/chord ./scripts/check-chord-contract.sh
```

CI runs unit coverage, race detection and companion contracts in separate jobs. The contract gate requires a buildable Chord checkout and cannot silently skip a missing companion. One immutable Chord CLI build is shared per source checkout per test process; do not modify that source or its build environment during a suite.

## Review the cost before expanding scope

Count production adapter code, shared code, tests and copied dependency code separately. A first text-only adapter should mostly normalize events and implement transport operations; several thousand lines of new infrastructure deserve a design review, not an automatic acceptance because tests pass. This is a review signal, not a hard line limit.

Ship text commands and replies first. Add cards, attachments, editable streaming previews or multi-enterprise installations as separate capabilities with concrete users and tests. Adding a platform should normally touch its adapter, factory/configuration registration and paired documentation; repeated edits to shared dispatch indicate a missing boundary. Keep runtime guarantees explicit: stronger restart guarantees are a product change, not incidental adapter work.
