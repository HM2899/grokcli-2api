# Anthropic Messages stream contracts

Scenarios served by `fake_upstream.py` (`X-Fake-Scenario` or `?scenario=`):

| Scenario | Expected Go/Python behavior |
|----------|-----------------------------|
| `normal` | text deltas + terminal finish + `[DONE]` |
| `tool-rewrite` | Update→Edit tool_use, dense index 0, stop_reason=tool_use |
| `thinking` | thinking block then text, dense indexes |
| `empty-stream` / `empty-200` | no model payload; failover before client envelope when possible |
| `slow` | idle gap allows keepalive ping / SSE comment |
| `truncate` | missing `[DONE]` still closes Anthropic envelope |

Stream invariants (from `manifest.json`):

1. Dense content block indexes
2. `message_delta` before `message_stop`
3. `tool_use` stop only after an emitted tool block
4. Soft-disconnect after envelope open still emits terminal frames
5. Keepalive: Anthropic `event: ping` and/or SSE comment during idle gaps


## Go e2e harness

`internal/server/messages_e2e_test.go` drives `POST /v1/messages` against an in-process
fake upstream that mirrors `contracts/fake_upstream.py` scenarios:

- normal non-stream text + observation headers
- stream thinking
- stream tool rewrite (Update→Edit)
- empty-stream multi-account failover
- affinity prefer + `X-Grok2API-Affinity`

Enable canary only after these and Python regressions stay green.


Operational canary steps: [`docs/GO_MESSAGES_CANARY.md`](../docs/GO_MESSAGES_CANARY.md).
