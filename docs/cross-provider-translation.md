# Cross-provider translation

Comporta translates conversations between Anthropic Messages, OpenAI Chat Completions and the conversational subset of the OpenAI Responses API. An app keeps the SDK and the format it already uses; the gateway renders each attempt in the protocol of the provider that receives it and converts the answer back to the client's protocol. This is what lets one fallback chain mix Anthropic, OpenAI and Ollama models, and what lets code written for the Responses API go through the gateway.

Translation never stores content. The request is converted in memory, from the body that was already capped, pseudonymized and inspected for sensitive data, and every candidate of a request is rendered from that same content.

## When a request is translated

| Client calls | Model's provider | What happens |
|---|---|---|
| `POST /v1/messages` | Anthropic | Native: the body goes as received, with the documented patches |
| `POST /v1/messages` | OpenAI or Ollama | Rendered as Chat Completions, answered as Messages |
| `POST /v1/chat/completions` | OpenAI or Ollama | Native |
| `POST /v1/chat/completions` | Anthropic | Rendered as Messages, answered as Chat Completions |
| `POST /v1/responses` | OpenAI or Ollama | Rendered as Chat Completions, answered as a Response object |
| `POST /v1/responses` | Anthropic | Rendered as Messages, answered as a Response object |

The native path is unchanged: unknown fields and the client's `anthropic-version` and `anthropic-beta` headers still reach a provider of the same format. A translated request carries no client header at all. Toward Anthropic it carries `anthropic-version` from the provider configuration (see below). `POST /v1/messages/count_tokens` is never translated and still refuses a model of another format with `provider_format_mismatch`.

A translated answer carries `x-comporta-translated`, which names the direction of the answer delivered:

| Value | Meaning |
|---|---|
| `openai->anthropic` | An OpenAI-format answer delivered to an Anthropic client |
| `anthropic->openai` | An Anthropic answer delivered to a Chat Completions or Responses client |
| `openai-chat->openai-responses` | A Chat Completions answer delivered to a Responses client |

On a cache hit the header is computed again from the current choice; it is never stored with the answer.

## Supported matrix

| Meaning | Anthropic Messages | OpenAI Chat | OpenAI Responses |
|---|---|---|---|
| Instructions | `system` | leading `system` and `developer` messages | `instructions`, leading `system` and `developer` items |
| Text | `text` block | string or `text` part | `input_text` and `output_text` |
| Image | `base64` or `url` source | `image_url` with a data URL or a URL | `input_image` with a data URL or a URL |
| Tool definition | `name`, `description`, `input_schema` | `function` tool | `function` tool |
| Tool call | `tool_use` | `tool_calls` | `function_call` item |
| Tool result | `tool_result` | `tool` message | `function_call_output` item |
| Tool choice | `auto`, `any`, `tool`, `none` | `auto`, `required`, named function, `none` | `auto`, `required`, named function, `none` |
| Parallel calls | `disable_parallel_tool_use` | `parallel_tool_calls` | `parallel_tool_calls` |
| Stop | `end_turn`, `max_tokens`, `tool_use`, `stop_sequence`, `refusal` | `stop`, `length`, `tool_calls`, `content_filter` | `completed`, or `incomplete` with `max_output_tokens` or `content_filter` |
| Output limit, temperature, top_p, stop sequences | supported | supported | output limit, temperature and top_p |
| Streaming | message and content block events | chunks and `[DONE]` | `response.*` events with sequence numbers |

Tool call IDs are kept when they only use letters, digits, `_` and `-`, which every protocol accepts, so a client can send them back on its next turn whichever provider answers. Any other ID becomes a deterministic synthetic one. Images are never downloaded: a URL stays a reference for the provider and inline data changes wrapper only. Anthropic accepts inline JPEG, PNG, GIF and WebP.

A few hints without an equivalent are dropped on purpose because they never change the answer: Anthropic `cache_control`, the `detail` of an OpenAI image, the Chat `metadata` of stored completions, and tool `strict` toward Anthropic. Everything else without an explicit rule makes the request ineligible for the other protocol instead of being dropped silently.

## Responses API subset

`POST /v1/responses` is authenticated like the OpenAI routes (`Authorization: Bearer <app key>`) and answers errors in the OpenAI format. It accepts `model`, `input` (a string or items), `instructions`, `max_output_tokens`, `temperature`, `top_p`, `stream`, function `tools`, `tool_choice`, `parallel_tool_calls`, `metadata`, `user` and `store: false`. Identity follows the usual precedence: `X-Comporta-User`, then `metadata.user_id`, then `user`; the app's `max_tokens` caps `max_output_tokens`.

The gateway keeps no state between requests, so the stateful parts of the API are outside the subset: `previous_response_id`, `conversation`, `store: true`, `background: true`, `include`, stored prompts, reasoning items and settings, structured output formats, built-in tools (web search, file search, computer use, code interpreter, MCP, image generation), files and audio. A request using any of them cannot be represented by any provider and ends with `no_eligible_fallback_model`. A malformed body, such as a missing `input` or an item without its type, is answered with 400 `invalid_request` before any model is chosen.

## Models that cannot represent a request

Before a model is quoted, reserved or sent, the gateway checks whether its protocol can carry the request without losing meaning. When it cannot, the model is skipped: it consumes no attempt, reservation, rate or budget, and does not appear in `x-comporta-fallback`. Up to five models are still sent per request. The skip is recorded as a durable `translation_skipped` event before the next model is considered; if that event cannot be written, the request ends with 503 `usage_storage_unavailable` and nothing else is sent.

```json
{
  "type": "translation_skipped",
  "app": "bot-discord",
  "payload": {
    "chain_position": 2,
    "model": "gpt-4.1",
    "source_format": "anthropic_messages",
    "target_format": "openai_chat",
    "reason": "untranslatable_feature",
    "feature": "provider_beta"
  }
}
```

`chain_position` is the model's place in the app's `fallback` list, starting at 1, or 0 for a requested model outside the list. The event has no user hash and no attempt index, and it does not feed alerts. `feature` is one of:

| Feature | Cause |
|---|---|
| `provider_beta` | The client sent `anthropic-beta`; no beta is assumed to exist elsewhere |
| `unknown_field` | A member without a translation rule |
| `unsupported_role` | A role outside user and assistant, or an instruction in the middle of a conversation |
| `unsupported_content` | A content part or block without an equivalent, such as a refusal or citations |
| `image_source` | An image the target cannot take, such as an inline BMP for Anthropic |
| `tool_type` | A server or hosted tool, or a legacy OpenAI function |
| `tool_choice` | A choice the target cannot honor, such as a required tool with no tools |
| `tool_result` | A tool result with an image or an error flag, toward Chat Completions |
| `structured_output` | A response format other than plain text |
| `reasoning` | Extended thinking, reasoning settings or reasoning content |
| `stateful_response` | Server-side state of the Responses API |
| `audio` | Audio input or output |
| `file` | Documents and files |
| `response_shape` | A protocol that is never a provider target |

When the requested model itself cannot represent the request, the first chain entry that can is used instead, before admission. When none can, the answer is HTTP 502:

```json
{"error":{"type":"server_error","message":"no eligible fallback model can represent this request (provider_beta)","comporta_code":"no_eligible_fallback_model"}}
```

The message names the closed feature only, never content, a schema, a URL or a field name.

## Answers, usage and streams

Usage is always read from the provider's own answer and settled with its counters, before the answer is converted. The client sees the same counters in its own format: Chat Completions and Responses report input including cached tokens, Messages reports them apart. A counter the provider did not report is not shown, except where the SDK types require a number, which then reads zero; the settlement never uses that projection.

A complete answer is validated in the provider's protocol and then converted. Reasoning, citations, refusals, annotations and server tool content are never turned into text: an answer holding them cannot be translated and ends with 502 `response_translation_failed`. The usage the provider reported is still settled, and the answer does not fall back to another model, since the failure is not an outage.

Streams are converted event by event, without buffering the answer. The first converted event is what commits the response: the gateway holds the opening event until the first content arrives, so a stream that starts with something untranslatable, such as a reasoning block, still gets an HTTP 502 instead of a broken stream. After the first event, a conversion error ends the stream with an error event in the client's format and no completion event: an `error` event for Messages and Responses clients, an `error` chunk without `[DONE]` for Chat Completions. A Responses client keeps the text of the answer in memory until the stream ends, because its done events and its final object repeat it; it is bounded by the 64 MB response limit.

## Anthropic API version

A translated request has no client `anthropic-version` header to forward, so an Anthropic provider sends its own:

```yaml
providers:
  anthropic:
    api_format: anthropic
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_API_KEY}
    anthropic_version: "2026-10-21"   # optional; only for api_format anthropic
    models: [claude-sonnet-4-5]
```

The default is `2026-10-21`. The value must be a date; setting it on an OpenAI provider is a configuration error. Native Anthropic requests keep the client's header.

## Mixed chain example

```yaml
apps:
  bot-discord:
    allowed_models: [claude-sonnet-4-5, gpt-4.1]
    fallback: [claude-sonnet-4-5, gpt-4.1]
```

An app using the Anthropic SDK with this chain keeps working during an Anthropic outage: after a 529 on `claude-sonnet-4-5`, the request goes to `gpt-4.1` as Chat Completions and the app receives an Anthropic message generated by OpenAI, with `x-comporta-fallback: claude-sonnet-4-5->gpt-4.1 (529)` and `x-comporta-translated: openai->anthropic`. [`examples/translation.yaml`](../examples/translation.yaml) runs the same chain on the fake provider, without credentials.

## Observability

Spans `translate_request` and `translate_response` time each conversion. `comporta_translations_total{source,target,result}` counts converted answers and `comporta_translation_skips_total{feature}` counts skipped models. Labels are closed sets: no model name outside the configuration, ID, URL, tool name or field ever becomes a label.
